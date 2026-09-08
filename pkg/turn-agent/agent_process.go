package turnagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	"go.opentelemetry.io/otel/attribute"
)

// Process executes one turn for the given rtc-queue Work item. Its signature
// matches rtcqueue.WorkerConfig.OnWork, so it plugs directly into rtc-queue's
// Worker.
//
// Process owns the turn's lifecycle. It creates / looks up the turn via
// Config callbacks, drives the eino TurnLoop, and calls the lifecycle
// callbacks (Begin/Resume/Complete/Interrupt/Fail/Cancel Turn) at the
// well-defined moments. Application code must NOT mutate turn state from
// other code paths.
//
// Process returns nil when the turn has reached a terminal state through the
// appropriate callback (Complete / Interrupt / Cancel). It returns a non-nil
// error when:
//   - The work payload could not be decoded
//   - CreateTurn / LookupTurn failed — the work is left in "processing"
//     status for admin recovery
//   - A start callback (BeginTurn / ResumeTurn) failed — same outcome
//   - The turn ended due to an unexpected error — FailTurn has been called,
//     and the error is propagated so rtc-queue keeps the work in "processing"
//   - The parent ctx was cancelled (graceful worker shutdown) — no lifecycle
//     callback is invoked, so the turn stays in its previous state and can be
//     picked up by another worker once the session lock expires
func (a *Agent) Process(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) (retErr error) {
	// 1. Decode payload — just {kind, sessionID}.
	var p WorkPayload
	if err := json.Unmarshal([]byte(work.Data), &p); err != nil {
		return fmt.Errorf("turnagent: decode work payload: %w", err)
	}
	if p.SessionID == "" {
		return fmt.Errorf("turnagent: work payload missing session_id")
	}
	a.logIfEnabled(ctx, LogLevelInfo, "agent.process", map[string]any{
		"p.SessionID": p.SessionID,
		"p.Kind":      p.Kind,
	})

	// 1.5. Fast path: compact work bypasses the turn loop entirely.
	if p.Kind == WorkKindCompact {
		if a.cfg.CompactContext == nil {
			a.logIfEnabled(ctx, LogLevelWarn, "agent.compact_no_handler", map[string]any{
				"p.SessionID": p.SessionID,
			})
			return nil
		}
		return a.cfg.CompactContext(ctx, p.SessionID, p.CustomInstruction)
	}

	// 2. Obtain the turnID.
	//
	// For submit: CreateTurn allocates a new turn (e.g., UUID + DB row).
	// For resume: LookupTurn finds the existing active turn for this session.
	//
	// turnID is then threaded through every subsequent callback for the
	// duration of this work — including across the eino Run(), so closures
	// in buildEinoConfig capture it.
	var (
		turnID string
		err    error
	)
	switch p.Kind {
	case WorkKindSubmit:
		// Pass work.ID as an idempotency key so CreateTurn can upsert on
		// (sessionID, workID) and tolerate rtc-queue retries.
		turnID, err = a.cfg.CreateTurn(ctx, p.SessionID, work.ID)
		if err != nil {
			return fmt.Errorf("turnagent: CreateTurn: %w", err)
		}
	case WorkKindResume:
		// workID is reserved for future idempotent resume. Implementations
		// may ignore it; the signature matches CreateTurn for symmetry.
		turnID, err = a.cfg.LookupTurn(ctx, p.SessionID, work.ID)
		if err != nil {
			// Check if the error is "no active turn" — this means the turn
			// was cancelled/completed between the resume work item being
			// published and processed. Complete the work gracefully.
			if errors.Is(err, ErrNoActiveTurn) {
				a.logIfEnabled(ctx, LogLevelInfo, "resume.no_active_turn", map[string]any{
					"session_id": p.SessionID,
					"work_id":    work.ID,
					"message":    "turn was cancelled/completed before resume",
				})
				return nil // Complete the work gracefully
			}
			return fmt.Errorf("turnagent: LookupTurn: %w", err)
		}
	default:
		return fmt.Errorf("turnagent: unknown work kind: %q", p.Kind)
	}

	// 2.5. Observability: start a turn span covering the entire Process
	// invocation from this point forward.
	turnCtx, turnSpan := a.startSpanIfEnabled(ctx, "turn")
	defer turnSpan.End()
	turnSpan.SetAttributes(
		attribute.String("session.id", p.SessionID),
		attribute.String("turn.id", turnID),
		attribute.String("turn.work_kind", string(p.Kind)),
	)
	turnStart := time.Now()
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.start", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
	})

	// 2.6. Panic recovery. rtc-queue's Worker does not recover panics from
	// OnWork, so the pkg must guard Process itself. Any panic in a user
	// callback is converted into a FailTurn transition so the turn reaches
	// a terminal state, and Process returns the recovered error so rtc-queue
	// marks the work in "processing" for admin recovery.
	//
	// The recovery is placed AFTER turnID is known so FailTurn can be called
	// with a valid ID. If the panic occurs during CreateTurn/LookupTurn
	// itself (before turnID exists), there is no turn to transition — the
	// recovered panic is returned as a raw error.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		var pErr error
		switch v := r.(type) {
		case error:
			pErr = fmt.Errorf("turnagent: panic in callback: %w", v)
		default:
			pErr = fmt.Errorf("turnagent: panic in callback: %v", v)
		}
		a.logIfEnabled(turnCtx, LogLevelError, "turn.panicked", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"error":      pErr.Error(),
		})
		// Best-effort transition to failed. Guard with an inner recover so a
		// misbehaving FailTurn does not swallow the original panic.
		if turnID != "" {
			func() {
				defer func() { _ = recover() }()
				_ = a.cfg.FailTurn(turnCtx, turnID, pErr)
			}()
		}
		retErr = pErr
	}()

	// 3. Get or create the session's long-running TurnLoop.
	//
	// Each session has a single long-running TurnLoop that persists across
	// multiple work items. This enables the "hold lock" mode.
	checkpointID := a.cfg.DeriveCheckpointID(p.SessionID)

	// Get or create the session loop.
	// The callback receives the SessionLoop so the TurnLoop's OnAgentEvents
	// references the same SessionLoop that PushAndWait waits on.
	sessionLoop, err := a.registry.GetOrCreate(turnCtx, p.SessionID, func(sl *SessionLoop) (*adk.TurnLoop[TurnWorkItem, *schema.Message], context.CancelFunc) {
		_, loopCancel := context.WithCancel(context.Background())

		// Build config with the existing SessionLoop reference
		einoCfg := a.buildEinoConfig(p.SessionID, checkpointID, sl)
		loop := adk.NewTurnLoop[TurnWorkItem, *schema.Message](einoCfg)

		sl.cancel = loopCancel
		return loop, loopCancel
	})
	if err != nil {
		return fmt.Errorf("turnagent: get or create session loop: %w", err)
	}

	// Reset lastMessage at the start of each turn
	sessionLoop.ResetLastMessage()

	// 4. Cancel listener.
	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var cancelledByQueue atomic.Bool
	var cancelReason string
	go func() {
		select {
		case cm := <-cancel:
			cancelReason = cm.Reason
			cancelledByQueue.Store(true)
			innerCancel()
			if a.cfg.Cancel.GracePeriod > 0 {
				sessionLoop.loop.Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				sessionLoop.loop.Stop(adk.WithImmediate())
			}
		case <-innerCtx.Done():
			return
		}
	}()

	// 5. Turn lifecycle: start.
	switch p.Kind {
	case WorkKindSubmit:
		if err := a.cfg.BeginTurn(turnCtx, turnID); err != nil {
			turnSpan.SetAttributes(attribute.String("turn.status", "error"))
			a.logIfEnabled(turnCtx, LogLevelError, "turn.begin_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: BeginTurn: %w", err)
		}
	case WorkKindResume:
		if err := a.cfg.ResumeTurn(turnCtx, turnID); err != nil {
			turnSpan.SetAttributes(attribute.String("turn.status", "error"))
			a.logIfEnabled(turnCtx, LogLevelError, "turn.resume_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: ResumeTurn: %w", err)
		}
	}

	// 6. Push payload into the session's long-running loop and wait for completion.
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.pushing_to_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
	})

	workItem := TurnWorkItem{
		WorkPayload: p,
		TurnID:      turnID,
	}

	pushErr := sessionLoop.PushAndWait(turnCtx, workItem)

	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.push_result", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
		"push_error": fmt.Sprintf("%v", pushErr),
	})

	// Convert to exit state for compatibility
	var exitReason error
	if pushErr != nil {
		exitReason = pushErr
	}

	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.turn_completed", map[string]any{
		"session_id":  p.SessionID,
		"turn_id":     turnID,
		"work_kind":   string(p.Kind),
		"exit_reason": fmt.Sprintf("%v", exitReason),
		"has_error":   exitReason != nil,
	})

	// 6.5. Reactive compact: TODO - temporarily disabled for long-running loop migration.
	if exitReason != nil && IsPromptTooLongError(exitReason) && a.cfg.RecoverFromPromptTooLong != nil {
		a.logIfEnabled(turnCtx, LogLevelWarn, "reactive_compact.disabled", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "reactive compact temporarily disabled for long-running loop migration",
		})
	}

	// 7. Turn lifecycle: end.
	//
	// Priority order matters: cancelledByQueue takes precedence over
	// exit.ExitReason, because an admin cancel may surface as a generic
	// context.Canceled or an eino CancelError.
	//
	// Observability: compute duration and status once, then emit to
	// Logger / Tracer / Metrics before invoking the terminal callback.
	turnDuration := time.Since(turnStart)

	recordEnd := func(status string, err error) {
		// Guard against panics from Logger / Tracer / Metrics implementations.
		// Observability must never break the turn's terminal transition.
		defer func() { _ = recover() }()
		turnSpan.SetAttributes(
			attribute.String("turn.status", status),
			attribute.Int64("turn.duration_ms", turnDuration.Milliseconds()),
		)
		if err != nil {
			turnSpan.RecordError(err)
		}
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.end", map[string]any{
			"session_id":  p.SessionID,
			"turn_id":     turnID,
			"work_kind":   string(p.Kind),
			"status":      status,
			"duration_ms": turnDuration.Milliseconds(),
			"error":       fmt.Sprintf("%v", err),
		})
		a.recordMetricIfEnabled(turnCtx, func(m Metrics) {
			m.RecordTurn(turnCtx, TurnMetricsAttrs{
				SessionID:  p.SessionID,
				TurnID:     turnID,
				WorkKind:   string(p.Kind),
				Status:     status,
				DurationMs: turnDuration.Milliseconds(),
				Error:      err,
			})
		})
	}

	if cancelledByQueue.Load() {
		recordEnd("cancel", nil)
		_ = a.cfg.CancelTurn(turnCtx, turnID, cancelReason)
		return nil
	}

	if errors.Is(innerCtx.Err(), context.Canceled) && ctx.Err() != nil {
		// Parent ctx cancelled (worker shutting down). Don't transition the
		// turn — leave it in its previous state so another worker can pick
		// up the work when the session lock expires. No terminal observability
		// emission: the turn didn't reach a terminal state.
		return ctx.Err()
	}

	switch {
	case exitReason == nil:
		// Clean exit. eino has deleted the checkpoint.
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.clean_exit", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "calling CompleteTurn",
		})
		recordEnd("success", nil)
		// Get the last message from sessionLoop for Sub Agent support
		lastMessage := sessionLoop.GetLastMessage()
		if err := a.cfg.CompleteTurn(turnCtx, p.SessionID, turnID, lastMessage); err != nil {
			a.logIfEnabled(turnCtx, LogLevelError, "turn.complete_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		} else {
			a.logIfEnabled(turnCtx, LogLevelInfo, "turn.completed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
			})
		}
		return nil

	case isInterruptError(exitReason):
		// Interrupt is a legitimate turn pause, not an error.
		var iErr *adk.InterruptError
		_ = errors.As(exitReason, &iErr)
		root := rootInterruptCtx(iErr.InterruptContexts)
		if root == nil {
			recordEnd("fail", exitReason)
			if err := a.cfg.FailTurn(turnCtx, turnID, exitReason); err != nil {
				a.logIfEnabled(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
					"session_id": p.SessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
				})
			}
			return exitReason
		}
		a.logIfEnabled(turnCtx, LogLevelInfo, "interrupt", map[string]any{
			"session_id":   p.SessionID,
			"turn_id":      turnID,
			"interrupt_id": root.ID,
			"reason":       "stateful_interrupt",
		})
		a.addEventIfEnabled(turnCtx, "interrupt",
			attribute.String("session.id", p.SessionID),
			attribute.String("turn.id", turnID),
			attribute.String("interrupt.id", root.ID),
			attribute.String("reason", "stateful_interrupt"),
		)
		a.recordMetricIfEnabled(turnCtx, func(m Metrics) {
			m.RecordInterrupt(turnCtx, InterruptMetricsAttrs{
				SessionID:   p.SessionID,
				TurnID:      turnID,
				InterruptID: root.ID,
				Reason:      "stateful_interrupt",
			})
		})
		recordEnd("interrupt", nil)
		if err := a.cfg.InterruptTurn(turnCtx, turnID, root.ID, root.Info); err != nil {
			a.logIfEnabled(turnCtx, LogLevelError, "turn.interrupt_callback_failed", map[string]any{
				"session_id":   p.SessionID,
				"turn_id":      turnID,
				"interrupt_id": root.ID,
				"error":        err.Error(),
			})
		}
		return nil

	default:
		// Unexpected error.
		recordEnd("fail", exitReason)
		if err := a.cfg.FailTurn(turnCtx, turnID, exitReason); err != nil {
			a.logIfEnabled(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return exitReason
	}
}
