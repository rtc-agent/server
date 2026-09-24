package turnagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/adk"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// isInterruptError checks if the error is an eino InterruptError.
func isInterruptError(err error) bool {
	var iErr *adk.InterruptError
	return errors.As(err, &iErr)
}

// rootInterruptCtx returns the root interrupt context — the deepest tool that
// actually raised the interrupt. Falls back to the first context if no root
// cause is marked. Returns nil if the slice is empty.
func rootInterruptCtx(ctxs []*adk.InterruptCtx) *adk.InterruptCtx {
	if len(ctxs) == 0 {
		return nil
	}
	for _, c := range ctxs {
		if c.IsRootCause {
			return c
		}
	}
	return ctxs[0]
}

// Process executes one turn for the given rtc-queue Work item. Its signature
// matches rtcqueue.WorkerConfig.OnWork, so it plugs directly into rtc-queue's
// Worker.
//
// V3 architecture: Process decodes the payload, creates/looks up the turn,
// then delegates to the SessionManagerRegistry. It pushes the work item to
// the session's TurnLoop and blocks until the work is completed by
// OnAgentEvents.
func (a *Agent) Process(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) (retErr error) {
	// 1. Decode payload.
	var p WorkPayload
	if err := json.Unmarshal([]byte(work.Data), &p); err != nil {
		return fmt.Errorf("turnagent: decode work payload: %w", err)
	}
	if p.SessionID == "" {
		return fmt.Errorf("turnagent: work payload missing session_id")
	}

	// 1.1. Restore trace context from payload (if available).
	// This must happen BEFORE startSpanIfEnabled (line ~93) so that the "turn" span
	// becomes a child of the restored span context, inheriting the same TraceID.
	// For legacy payloads without trace_id, this is a no-op (empty strings are ignored).
	if p.TraceID != "" && p.SpanID != "" {
		ctx = WithTraceContext(ctx, p.TraceID, p.SpanID)
	}

	a.log(ctx, LogLevelInfo, "agent.process", map[string]any{
		"session_id": p.SessionID,
		"kind":       p.Kind,
		"work_id":    work.ID,
	})

	// 1.5. Fast path: compact work bypasses the turn loop entirely.
	if p.Kind == WorkKindCompact {
		if a.cfg.CompactContext == nil {
			a.log(ctx, LogLevelWarn, "agent.compact_no_handler", map[string]any{
				"session_id": p.SessionID,
			})
			return nil
		}
		return a.cfg.CompactContext(ctx, p.SessionID, p.CustomInstruction)
	}

	// 2. Obtain the turnID.
	turnID, err := a.resolveTurnID(ctx, p.SessionID, work.ID, p.Kind)
	if err != nil {
		if p.Kind == WorkKindResume && errors.Is(err, ErrNoActiveTurn) {
			// This can indicate a race condition (turn cancelled between resume
			// publish and processing) or Checkpoint corruption. Use Warn so it
			// shows up in production logs for investigation.
			a.log(ctx, LogLevelWarn, "resume.no_active_turn", map[string]any{
				"session_id":   p.SessionID,
				"work_id":      work.ID,
				"message":      "turn was cancelled/completed before resume — possible race condition or checkpoint corruption",
				"interrupt_id": p.InterruptID,
			})
			return nil
		}
		return fmt.Errorf("turnagent: resolveTurnID: %w", err)
	}

	// 2.5. Observability: start a turn span.
	// Create turnCtx with its own cancel so we can cancel the entire turn
	// (including the LLM call running in mgr.Run) when StopTurn is called.
	turnCtx, turnCancel := context.WithCancel(ctx)
	var turnSpan trace.Span
	turnCtx, turnSpan = a.startSpanIfEnabled(turnCtx, "turn")
	// Enrich context with sessionID so ALL callbacks (not just beginTurn) can
	// fall back to it when DB lookups fail (e.g., failTurn/cancelTurn session
	// status update). Without this, a GetByID failure after a terminal turn
	// status update leaves the session stuck at "active" until the stale
	// turn scanner runs (5-30 minutes).
	turnCtx = WithSessionID(turnCtx, p.SessionID)
	defer turnCancel()
	defer turnSpan.End()
	turnSpan.SetAttributes(
		attribute.String("session.id", p.SessionID),
		attribute.String("turn.id", turnID),
		attribute.String("turn.work_kind", string(p.Kind)),
	)
	turnStart := time.Now()
	a.log(turnCtx, LogLevelInfo, "turn.start", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
		"work_id":    work.ID,
	})

	// 2.6. Panic recovery.
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
		a.log(turnCtx, LogLevelError, "turn.panicked", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"error":      pErr.Error(),
			"stack":      string(debug.Stack()),
		})
		if turnID != "" {
			func() {
				// Nested recover: FailTurn may panic (e.g., DB unreachable).
				// Swallow the panic to prevent it from crashing the process
				// during an already-in-flight panic recovery.
				defer func() { _ = recover() }()
				_ = a.cfg.FailTurn(turnCtx, turnID, pErr)
			}()
		}
		retErr = pErr
	}()

	// 3. Checkpoint ID.
	checkpointID := a.cfg.DeriveCheckpointID(p.SessionID)

	// 4. Get or create a SessionTurnManager.
	logFn := func(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
		a.log(ctx, level, msg, fields)
	}

	mgr, isNew, err := a.registry.GetOrCreate(
		turnCtx, a.queue, p.SessionID, a.workerID, turnID, checkpointID,
		work.Credential, a.cfg, logFn,
	)
	if err != nil {
		// Claim failed. This could mean the session is locked by another worker
		// or the queue is empty. Return the error so rtc-queue can handle it.
		a.log(turnCtx, LogLevelWarn, "turn.get_or_create_failed", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"error":      err.Error(),
		})
		return fmt.Errorf("turnagent: GetOrCreate: %w", err)
	}

	// 5. Build work item and push to the loop.
	workItem := TurnWorkItem{
		WorkPayload: p,
		TurnID:      turnID,
		WorkID:      work.ID,
	}

	mgr, isNew, err = a.pushOrReplace(turnCtx, mgr, isNew, workItem, p.SessionID, turnID, checkpointID, work.Credential)
	if err != nil {
		return err
	}

	// If this is a new manager, begin the turn.
	if isNew {
		// Enrich context with sessionID so callbacks can use it as a fallback
		// when DB lookups fail (e.g., beginTurn session activation).
		beginCtx := WithSessionID(turnCtx, p.SessionID)
		if err := a.beginTurn(beginCtx, turnSpan, p.SessionID, turnID, p.Kind); err != nil {
			return err
		}
	}

	// 6. Cancel listener.
	innerCtx, innerCancel := context.WithCancel(turnCtx)
	defer innerCancel()

	done := make(chan struct{})
	defer close(done)
	a.startCancelListener(cancel, mgr, done, innerCancel, turnCancel)

	// 7. Register work with tracker and wait for completion.
	completionCh := mgr.Tracker().Register(work.ID)

	a.log(turnCtx, LogLevelInfo, "turn.waiting_completion", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_id":    work.ID,
	})

	// Wait for either:
	// - The work item to be completed (OnAgentEvents calls tracker.Complete)
	// - The context to be cancelled (worker shutdown)
	select {
	case <-completionCh:
		a.log(turnCtx, LogLevelInfo, "turn.work_completed", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"work_id":    work.ID,
		})
	case <-innerCtx.Done():
		a.log(turnCtx, LogLevelInfo, "turn.ctx_done_waiting", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"work_id":    work.ID,
		})
	}

	// 8. Wait for the loop to exit and perform lifecycle transitions.
	// Only the first Process() for a session (isNew=true) handles the full
	// lifecycle. Subsequent Process() calls for the same session just wait
	// for their work item completion and return.
	if !isNew {
		return a.handleNonOwnerCompletion(
			turnCtx, mgr, completionCh, work.ID,
			p.SessionID, turnID,
		)
	}

	return a.handleOwnerLifecycleEnd(turnCtx, turnSpan, innerCtx, mgr, p, turnID, turnStart, work.ID)
}
