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

	// 3. Build eino config and TurnLoop.
	//
	// buildEinoConfig captures sessionID and turnID in closures, so
	// GenInput / GenResume / PrepareAgent / OnAgentEvents can pass them to
	// the application's data callbacks without needing them in the eino
	// item type.
	checkpointID := a.cfg.DeriveCheckpointID(p.SessionID)
	einoCfg := a.buildEinoConfig(p.SessionID, turnID, checkpointID)
	loop := adk.NewTurnLoop[WorkPayload, *schema.Message](einoCfg)

	// 4. Cancel listener.
	//
	// An inner context is used so we can cancel eino's Run without affecting
	// the parent ctx (which the rtc-queue Worker owns). cancelledByQueue
	// disambiguates "explicit admin cancel" from "parent ctx cancelled by
	// worker shutdown" — only the former should transition the turn to
	// "cancelled".
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
			// Build stop options based on Cancel config.
			//
			// GracePeriod > 0 → graceful: wait up to GracePeriod for the
			// current model/tool call to finish at a safe point, then stop.
			// This avoids half-generated responses visible to the frontend.
			//
			// GracePeriod == 0 (default) → immediate: abort as soon as
			// possible. Use this for admin cancels where responsiveness
			// matters more than output coherence.
			if a.cfg.Cancel.GracePeriod > 0 {
				loop.Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				loop.Stop(adk.WithImmediate())
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

	// 6. Push payload into eino's buffer and run.
	//
	// The payload itself carries only {kind, sessionID}; eino uses it only
	// as a trigger. Actual per-turn data flows through the closures in
	// buildEinoConfig (which have sessionID/turnID) and the data callbacks
	// (LoadMessages etc.).
	//
	// For Submit: eino calls GenInput (no checkpoint exists).
	// For Resume: eino finds the checkpoint, calls GenResume instead.
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.pushing_to_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
	})
	pushed, pushAck := loop.Push(p)
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.push_result", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
		"pushed":     pushed,
		"has_ack":    pushAck != nil,
	})
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.running_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
	})
	loop.Run(turnCtx)
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.waiting_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
	})
	exit := loop.Wait()

	// Debug: log eino exit reason to trace why turn didn't complete
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.eino_exited", map[string]any{
		"session_id":  p.SessionID,
		"turn_id":     turnID,
		"work_kind":   string(p.Kind),
		"exit_reason": fmt.Sprintf("%v", exit.ExitReason),
		"has_error":   exit.ExitReason != nil,
	})

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
	case exit.ExitReason == nil:
		// Clean exit. eino has deleted the checkpoint.
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.clean_exit", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "calling CompleteTurn",
		})
		recordEnd("success", nil)
		if err := a.cfg.CompleteTurn(turnCtx, turnID); err != nil {
			// Terminal callback error: log it, but the turn has reached a
			// terminal state from eino's perspective. Return nil so rtc-queue
			// marks the work as complete; the DB inconsistency is for admin
			// reconciliation, not a reason to retry the turn.
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

	case isInterruptError(exit.ExitReason):
		// Interrupt is a legitimate turn pause, not an error.
		// eino has persisted the checkpoint.
		var iErr *adk.InterruptError
		_ = errors.As(exit.ExitReason, &iErr)
		root := rootInterruptCtx(iErr.InterruptContexts)
		if root == nil {
			// Defensive: shouldn't happen for a well-formed InterruptError,
			// but guard anyway so we don't panic below.
			recordEnd("fail", exit.ExitReason)
			if err := a.cfg.FailTurn(turnCtx, turnID, exit.ExitReason); err != nil {
				a.logIfEnabled(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
					"session_id": p.SessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
				})
			}
			// The turn has reached a terminal state from eino's perspective;
			// return the underlying error so rtc-queue leaves the work in
			// "processing" for admin recovery.
			return exit.ExitReason
		}
		// Observability: interrupt event.
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
			// Terminal callback error: log it, but the turn has paused.
			// Return nil so rtc-queue marks the work as complete; the DB
			// inconsistency is for admin reconciliation.
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
		recordEnd("fail", exit.ExitReason)
		if err := a.cfg.FailTurn(turnCtx, turnID, exit.ExitReason); err != nil {
			a.logIfEnabled(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		// The turn has reached a terminal state from eino's perspective;
		// return the underlying error so rtc-queue leaves the work in
		// "processing" for admin recovery.
		return exit.ExitReason
	}
}
