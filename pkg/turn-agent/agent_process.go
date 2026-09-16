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
	turnCtx, turnSpan := a.startSpanIfEnabled(turnCtx, "turn")
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

	pushed, _ := mgr.Loop().Push(workItem)
	if !pushed {
		// Loop stopped. Try to replace the manager.
		a.log(turnCtx, LogLevelInfo, "turn.push_failed_replacing", map[string]any{
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
		a.log(turnCtx, LogLevelWarn, "turn.owner_work_abandoned", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"work_id":    work.ID,
			"message":    "owner's work was not processed before loop exited, requeuing",
		})
		if reErr := a.queue.RequeueWork(context.Background(), work.ID); reErr != nil {
			a.log(turnCtx, LogLevelWarn, "turn.requeue_owner_failed", map[string]any{
				"work_id": work.ID,
				"error":   reErr.Error(),
			})
		}
	}

	a.log(turnCtx, LogLevelInfo, "turn.loop_exited", map[string]any{
		"session_id":  p.SessionID,
		"turn_id":     turnID,
		"work_kind":   string(p.Kind),
		"exit_reason": fmt.Sprintf("%v", exitState.ExitReason),
		"has_error":   exitState.ExitReason != nil,
	})

	exitReason := exitState.ExitReason

	// 9. Turn lifecycle: end.
	turnDuration := time.Since(turnStart)

	if mgr.IsCancelledByQueue() {
		a.recordTurnEnd(turnCtx, turnSpan, p.SessionID, turnID, string(p.Kind), turnDuration, "cancel", nil)
		_ = a.cfg.CancelTurn(turnCtx, turnID, mgr.CancelReason())
		return nil
	}

	if errors.Is(innerCtx.Err(), context.Canceled) && turnCtx.Err() != nil {
		return turnCtx.Err()
	}

	switch {
	case exitReason == nil:
		a.log(turnCtx, LogLevelInfo, "turn.clean_exit", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "calling CompleteTurn",
		})
		a.recordTurnEnd(turnCtx, turnSpan, p.SessionID, turnID, string(p.Kind), turnDuration, "success", nil)
		if err := a.cfg.CompleteTurn(turnCtx, p.SessionID, turnID, mgr.LastMessage()); err != nil {
			a.log(turnCtx, LogLevelError, "turn.complete_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return nil

	case isInterruptError(exitReason):
		return a.handleInterruptExit(turnCtx, turnSpan, exitReason, p.SessionID, turnID)

	default:
		// Reactive compact: prompt-too-long retry logic.
		if IsPromptTooLongError(exitReason) && a.cfg.RecoverFromPromptTooLong != nil {
			maxAttempts := a.cfg.MaxReactiveCompactAttempts
			recoveryPublished := false
			for attempt := 1; attempt <= maxAttempts && !recoveryPublished; attempt++ {
				a.log(turnCtx, LogLevelWarn, "turn.prompt_too_long_recovering", map[string]any{
					"session_id": p.SessionID,
					"turn_id":    turnID,
					"attempt":    attempt,
					"max":        maxAttempts,
				})

				// Step 1: call reactive compact callback to compress context.
				if recoverErr := a.cfg.RecoverFromPromptTooLong(turnCtx, p.SessionID, attempt); recoverErr != nil {
					a.log(turnCtx, LogLevelError, "turn.reactive_compact_failed", map[string]any{
						"error": recoverErr.Error(),
					})
					break // compression failed, fall through to FailTurn
				}

				// Step 2: insert "compressing" feedback message.
				if a.cfg.InsertFeedbackMessage != nil {
					_ = a.cfg.InsertFeedbackMessage(turnCtx, p.SessionID, turnID,
						"context",
						"上下文超出限制",
						"对话内容太长，系统正在自动压缩后重试。请稍等片刻。",
						true, "")
				}

				// Step 3: end current Turn (FailTurn must be before Publish).
				// Use WithSkipErrorMessage to prevent failTurn callback from
				// inserting a duplicate error message.
				a.recordTurnEnd(turnCtx, turnSpan, p.SessionID, turnID, string(p.Kind), turnDuration, "failed", fmt.Errorf("prompt_too_long_recovering"))
				skipCtx := WithSkipErrorMessage(turnCtx)
				if err := a.cfg.FailTurn(skipCtx, turnID, exitReason); err != nil {
					// NOTE: Unlike the normal FailTurn path (below), we log but do NOT
					// propagate this error. The reactive compact path has already compressed
					// the context and is about to publish a new work item; stopping here
					// would waste the compression work and leave the user stuck. The new
					// Process will create a fresh turn regardless of the old turn's DB state.
					a.log(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
						"error": err.Error(),
					})
				}

				// Step 4: publish submit work item to trigger a new Process lifecycle.
				// Use context.Background() because turnCtx may be cancelled.
				payload := string(MarshalSubmitPayload(p.SessionID, attempt))
				if _, err := a.queue.Publish(context.Background(), p.SessionID, payload, 100); err != nil {
					a.log(turnCtx, LogLevelError, "turn.submit_publish_failed", map[string]any{
						"error":   err.Error(),
						"attempt": attempt,
					})
					break
				}

				// Recovery published successfully — set flag so loop condition exits.
				recoveryPublished = true
			}
			if recoveryPublished {
				// Return nil so rtc-queue marks this work complete; the new Process
				// (triggered by the published submit work item) handles the retried turn.
				return nil
			}
		}

		// Attempts exhausted or non-prompt-too-long: execute original FailTurn logic.
		a.recordTurnEnd(turnCtx, turnSpan, p.SessionID, turnID, string(p.Kind), turnDuration, "fail", exitReason)
		if err := a.cfg.FailTurn(turnCtx, turnID, exitReason); err != nil {
			a.log(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return exitReason
	}
}
