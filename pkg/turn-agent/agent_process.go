package turnagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	"go.opentelemetry.io/otel/attribute"
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
	a.logIfEnabled(ctx, LogLevelInfo, "agent.process", map[string]any{
		"p.SessionID": p.SessionID,
		"p.Kind":      p.Kind,
		"work_id":     work.ID,
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
	var (
		turnID string
		err    error
	)
	switch p.Kind {
	case WorkKindSubmit:
		turnID, err = a.cfg.CreateTurn(ctx, p.SessionID, work.ID)
		if err != nil {
			return fmt.Errorf("turnagent: CreateTurn: %w", err)
		}
	case WorkKindResume:
		turnID, err = a.cfg.LookupTurn(ctx, p.SessionID, work.ID)
		if err != nil {
			if errors.Is(err, ErrNoActiveTurn) {
				a.logIfEnabled(ctx, LogLevelInfo, "resume.no_active_turn", map[string]any{
					"session_id": p.SessionID,
					"work_id":    work.ID,
					"message":    "turn was cancelled/completed before resume",
				})
				return nil
			}
			return fmt.Errorf("turnagent: LookupTurn: %w", err)
		}
	default:
		return fmt.Errorf("turnagent: unknown work kind: %q", p.Kind)
	}

	// 2.5. Observability: start a turn span.
	// Create turnCtx with its own cancel so we can cancel the entire turn
	// (including the LLM call running in mgr.Run) when StopTurn is called.
	turnCtx, turnCancel := context.WithCancel(ctx)
	turnCtx, turnSpan := a.startSpanIfEnabled(turnCtx, "turn")
	defer turnCancel()
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
		a.logIfEnabled(turnCtx, LogLevelError, "turn.panicked", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"error":      pErr.Error(),
		})
		if turnID != "" {
			func() {
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
		a.logIfEnabled(ctx, level, msg, fields)
	}

	mgr, isNew, err := a.registry.GetOrCreate(
		turnCtx, a.queue, p.SessionID, a.workerID, turnID, checkpointID,
		work.Credential, a.cfg, logFn,
	)
	if err != nil {
		// Claim failed. This could mean the session is locked by another worker
		// or the queue is empty. Return the error so rtc-queue can handle it.
		a.logIfEnabled(turnCtx, LogLevelWarn, "turn.get_or_create_failed", map[string]any{
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

	pushed, _ := mgr.Loop().Push(workItem)
	if !pushed {
		// Loop stopped. Try to replace the manager.
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.push_failed_replacing", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
		})

		var replacedIsNew bool
		mgr, replacedIsNew, err = a.registry.Replace(
			turnCtx, a.queue, p.SessionID, a.workerID, turnID, checkpointID,
			mgr, work.Credential, a.cfg, logFn,
		)
		if err != nil {
			return fmt.Errorf("turnagent: Replace: %w", err)
		}

		// The caller is now the owner of the session's turn lifecycle
		// (analogous to isNew=true from GetOrCreate).
		if replacedIsNew {
			isNew = true
		}

		pushed, _ = mgr.Loop().Push(workItem)
		if !pushed {
			return fmt.Errorf("turnagent: failed to push work item after replacement")
		}
	}

	// If this is a new manager, begin the turn.
	if isNew {
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
	}

	// 6. Cancel listener.
	innerCtx, innerCancel := context.WithCancel(turnCtx)
	defer innerCancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case cm := <-cancel:
			mgr.SetCancelledByQueue(cm.Reason)
			if a.cfg.Cancel.GracePeriod > 0 {
				mgr.Loop().Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				mgr.Loop().Stop(adk.WithImmediate())
			}
			innerCancel()
			turnCancel() // Also cancel turnCtx to stop the LLM call in mgr.Run
		case <-done:
			return
		}
	}()

	// 7. Register work with tracker and wait for completion.
	completionCh := mgr.Tracker().Register(work.ID)

	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.waiting_completion", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_id":    work.ID,
	})

	// Wait for either:
	// - The work item to be completed (OnAgentEvents calls tracker.Complete)
	// - The context to be cancelled (worker shutdown)
	select {
	case <-completionCh:
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.work_completed", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"work_id":    work.ID,
		})
	case <-innerCtx.Done():
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.ctx_done_waiting", map[string]any{
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
		// Not the owner of this session's lifecycle.
		if mgr.IsCancelledByQueue() {
			return nil // CancelTurn is handled by the owning Process.
		}

		// Check if the work was actually completed. The select above may have
		// picked innerCtx.Done() even when completionCh was also ready (Go's
		// select is non-deterministic). Check completionCh non-blocking first
		// to avoid falsely reporting the work as abandoned.
		select {
		case <-completionCh:
			// Work was actually completed — the innerCtx.Done() path was a
			// false alarm (e.g., CompleteAll closed the channel at the same
			// time the cancel listener called innerCancel).
			a.logIfEnabled(turnCtx, LogLevelInfo, "turn.work_completed_deferred", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"work_id":    work.ID,
			})
			return nil
		default:
		}

		// Wait for the manager's cleanup to complete before checking IsAbandoned.
		// CompleteAll() runs during doCleanup (before done is closed) and sets the
		// allDone flag that IsAbandoned relies on. Without this wait, there is a
		// race: the non-owner could check IsAbandoned before CompleteAll runs, get
		// false (entry still in pending map), and return nil — leaving the work
		// stuck in "processing" status in Redis (ghost work).
		//
		// mgr.Done() closes after doCleanup completes, which includes CompleteAll.
		// After Done() closes, IsAbandoned gives a definitive answer.
		select {
		case <-completionCh:
			// Work completed during the wait — not abandoned.
			a.logIfEnabled(turnCtx, LogLevelInfo, "turn.work_completed_during_wait", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"work_id":    work.ID,
			})
			return nil
		case <-mgr.Done():
			// Manager cleanup complete — CompleteAll has run, IsAbandoned is definitive.
		}

		// Check if the work was actually completed (Complete was called by
		// OnAgentEvents) vs. abandoned (CompleteAll was called during manager
		// shutdown). When abandoned, the work is still in "processing" status
		// in Redis — requeue it so another worker can pick it up.
		if mgr.Tracker().IsAbandoned(work.ID) {
			a.logIfEnabled(turnCtx, LogLevelWarn, "turn.work_abandoned", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"work_id":    work.ID,
				"message":    "manager shut down before work was processed, requeuing",
			})
			// Use context.Background() to ensure requeue succeeds even if
			// the worker context is being cancelled.
			if reErr := a.queue.RequeueWork(context.Background(), work.ID); reErr != nil {
				a.logIfEnabled(turnCtx, LogLevelWarn, "turn.requeue_abandoned_failed", map[string]any{
					"work_id": work.ID,
					"error":   reErr.Error(),
				})
			}
			return fmt.Errorf("turnagent: work %s abandoned: manager shut down before processing", work.ID)
		}

		return nil
	}

	// Wait for loop to exit.
	exitState := mgr.Wait()

	// Perform cleanup (claim remaining work, release lock, remove from registry).
	mgr.Cleanup(turnCtx)

	// Check if the owner's own work was abandoned (pushed to the loop but never
	// processed by OnAgentEvents). This can happen when the loop exits due to lock
	// loss, context cancellation, or other errors before the work item is consumed.
	// The work is still in "processing" state in Redis — requeue it so another
	// worker can pick it up.
	if mgr.Tracker().IsAbandoned(work.ID) {
		a.logIfEnabled(turnCtx, LogLevelWarn, "turn.owner_work_abandoned", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"work_id":    work.ID,
			"message":    "owner's work was not processed before loop exited, requeuing",
		})
		if reErr := a.queue.RequeueWork(context.Background(), work.ID); reErr != nil {
			a.logIfEnabled(turnCtx, LogLevelWarn, "turn.requeue_owner_failed", map[string]any{
				"work_id": work.ID,
				"error":   reErr.Error(),
			})
		}
	}

	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.loop_exited", map[string]any{
		"session_id":  p.SessionID,
		"turn_id":     turnID,
		"work_kind":   string(p.Kind),
		"exit_reason": fmt.Sprintf("%v", exitState.ExitReason),
		"has_error":   exitState.ExitReason != nil,
	})

	exitReason := exitState.ExitReason

	// 9. Turn lifecycle: end.
	turnDuration := time.Since(turnStart)

	recordEnd := func(status string, err error) {
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

	if mgr.IsCancelledByQueue() {
		recordEnd("cancel", nil)
		_ = a.cfg.CancelTurn(turnCtx, turnID, mgr.CancelReason())
		return nil
	}

	if errors.Is(innerCtx.Err(), context.Canceled) && turnCtx.Err() != nil {
		return turnCtx.Err()
	}

	switch {
	case exitReason == nil:
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.clean_exit", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "calling CompleteTurn",
		})
		recordEnd("success", nil)
		if err := a.cfg.CompleteTurn(turnCtx, p.SessionID, turnID, mgr.LastMessage()); err != nil {
			a.logIfEnabled(turnCtx, LogLevelError, "turn.complete_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return nil

	case isInterruptError(exitReason):
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
