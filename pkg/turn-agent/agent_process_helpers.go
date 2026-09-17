package turnagent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/adk"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleNonOwnerCompletion handles the completion path for non-owner Process
// calls (isNew=false). Non-owners do not manage the session's turn lifecycle;
// they only wait for their work item to complete or be abandoned.
func (a *Agent) handleNonOwnerCompletion(
	ctx context.Context,
	mgr *SessionTurnManager,
	completionCh <-chan struct{},
	workID, sessionID, turnID string,
) error {
	if mgr.IsCancelledByQueue() {
		return nil // CancelTurn is handled by the owning Process.
	}

	// Check if the work was actually completed. The select in Process may have
	// picked innerCtx.Done() even when completionCh was also ready (Go's
	// select is non-deterministic). Check completionCh non-blocking first
	// to avoid falsely reporting the work as abandoned.
	select {
	case <-completionCh:
		a.log(ctx, LogLevelInfo, "turn.work_completed_deferred", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"work_id":    workID,
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
		a.log(ctx, LogLevelInfo, "turn.work_completed_during_wait", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"work_id":    workID,
		})
		return nil
	case <-mgr.Done():
		// Manager cleanup complete — CompleteAll has run, IsAbandoned is definitive.
	}

	// Check if the work was actually completed (Complete was called by
	// OnAgentEvents) vs. abandoned (CompleteAll was called during manager
	// shutdown). When abandoned, the work is still in "processing" status
	// in Redis — requeue it so another worker can pick it up.
	if mgr.Tracker().IsAbandoned(workID) {
		a.log(ctx, LogLevelWarn, "turn.work_abandoned", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"work_id":    workID,
			"message":    "manager shut down before work was processed, requeuing",
		})
		if reErr := a.queue.RequeueWork(context.Background(), workID); reErr != nil {
			a.log(ctx, LogLevelWarn, "turn.requeue_abandoned_failed", map[string]any{
				"work_id": workID,
				"error":   reErr.Error(),
			})
		}
		return fmt.Errorf("turnagent: work %s abandoned: manager shut down before processing", workID)
	}

	return nil
}

// recordTurnEnd records turn completion metrics, tracing, and logging.
func (a *Agent) recordTurnEnd(
	ctx context.Context,
	span trace.Span,
	sessionID, turnID, workKind string,
	duration time.Duration,
	status string,
	err error,
) {
	defer func() { _ = recover() }()
	span.SetAttributes(
		attribute.String("turn.status", status),
		attribute.Int64("turn.duration_ms", duration.Milliseconds()),
	)
	if err != nil {
		span.RecordError(err)
	}
	fields := map[string]any{
		"session_id":  sessionID,
		"turn_id":     turnID,
		"work_kind":   workKind,
		"status":      status,
		"duration_ms": duration.Milliseconds(),
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	a.log(ctx, LogLevelInfo, "turn.end", fields)
	a.recordMetricIfEnabled(func(m Metrics) {
		m.RecordTurn(ctx, TurnMetricsAttrs{
			SessionID:  sessionID,
			TurnID:     turnID,
			WorkKind:   workKind,
			Status:     status,
			DurationMs: duration.Milliseconds(),
			Error:      err,
		})
	})
}

// handleInterruptExit handles the case where the turn loop exited due to an
// eino interrupt. It converts the eino interrupt context to our internal type,
// records metrics/tracing, and delegates to InterruptTurn.
func (a *Agent) handleInterruptExit(
	ctx context.Context,
	span trace.Span,
	exitReason error,
	sessionID, turnID string,
) error {
	var iErr *adk.InterruptError
	if !errors.As(exitReason, &iErr) || iErr == nil {
		// Defensive: caller guards with isInterruptError, so this should
		// never happen. But handle it gracefully to prevent nil dereference
		// if the guard is ever relaxed.
		a.log(ctx, LogLevelError, "handle_interrupt_exit.not_interrupt_error", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
		})
		a.recordTurnEnd(ctx, span, sessionID, turnID, "", 0, "fail", exitReason)
		if err := a.cfg.FailTurn(ctx, turnID, exitReason); err != nil {
			a.log(ctx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return exitReason
	}
	root := rootInterruptCtx(iErr.InterruptContexts)
	if root == nil {
		a.recordTurnEnd(ctx, span, sessionID, turnID, "", 0, "fail", exitReason)
		if err := a.cfg.FailTurn(ctx, turnID, exitReason); err != nil {
			a.log(ctx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return exitReason
	}

	// Convert eino's InterruptCtx to our InterruptContext type.
	allContexts := make([]*InterruptContext, 0, len(iErr.InterruptContexts))
	for _, c := range iErr.InterruptContexts {
		allContexts = append(allContexts, &InterruptContext{
			ID:   c.ID,
			Info: c.Info,
		})
	}

	a.log(ctx, LogLevelInfo, "interrupt", map[string]any{
		"session_id":      sessionID,
		"turn_id":         turnID,
		"interrupt_id":    root.ID,
		"interrupt_count": len(allContexts),
		"reason":          "stateful_interrupt",
	})
	a.addEventIfEnabled(ctx, "interrupt",
		attribute.String("session.id", sessionID),
		attribute.String("turn.id", turnID),
		attribute.String("interrupt.id", root.ID),
		attribute.Int("interrupt.count", len(allContexts)),
		attribute.String("reason", "stateful_interrupt"),
	)
	a.recordMetricIfEnabled(func(m Metrics) {
		m.RecordInterrupt(ctx, InterruptMetricsAttrs{
			SessionID:   sessionID,
			TurnID:      turnID,
			InterruptID: root.ID,
			Reason:      "stateful_interrupt",
		})
	})
	a.recordTurnEnd(ctx, span, sessionID, turnID, "", 0, "interrupt", nil)
	if err := a.cfg.InterruptTurn(ctx, turnID, root.ID, root.Info, allContexts); err != nil {
		a.log(ctx, LogLevelError, "turn.interrupt_callback_failed", map[string]any{
			"session_id":   sessionID,
			"turn_id":      turnID,
			"interrupt_id": root.ID,
			"error":        err.Error(),
		})
	}
	return nil
}

// pushOrReplace pushes a work item to the manager's loop. If the loop has
// stopped (push fails), it replaces the manager in the registry and retries.
// Returns the (possibly replaced) manager, whether the caller is the owner,
// and any error.
func (a *Agent) pushOrReplace(
	ctx context.Context,
	mgr *SessionTurnManager,
	isNew bool,
	workItem TurnWorkItem,
	sessionID, turnID, checkpointID, credential string,
) (*SessionTurnManager, bool, error) {
	pushed, _ := mgr.Loop().Push(workItem)
	if pushed {
		return mgr, isNew, nil
	}

	// Loop stopped. Try to replace the manager.
	a.log(ctx, LogLevelInfo, "turn.push_failed_replacing", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
	})

	replacedMgr, replacedIsNew, err := a.registry.Replace(
		ctx, a.queue, sessionID, a.workerID, turnID, checkpointID,
		mgr, credential, a.cfg,
		func(c context.Context, level LogLevel, msg string, fields map[string]any) {
			a.log(c, level, msg, fields)
		},
	)
	if err != nil {
		return nil, false, fmt.Errorf("turnagent: Replace: %w", err)
	}

	if replacedIsNew {
		isNew = true
	}

	pushed, _ = replacedMgr.Loop().Push(workItem)
	if !pushed {
		return nil, false, fmt.Errorf("turnagent: failed to push work item after replacement")
	}

	return replacedMgr, isNew, nil
}

// handleOwnerLifecycleEnd manages the post-loop lifecycle for the owning
// Process call (isNew=true). It waits for the loop to exit, performs cleanup,
// handles abandoned work, and dispatches to the appropriate turn-end handler
// (cancel, interrupt, clean exit, or fail with optional reactive compact).
func (a *Agent) handleOwnerLifecycleEnd(
	ctx context.Context,
	span trace.Span,
	innerCtx context.Context,
	mgr *SessionTurnManager,
	p WorkPayload,
	turnID string,
	turnStart time.Time,
	workID string,
) error {
	// Wait for loop to exit.
	exitState := mgr.Wait()

	// Perform cleanup (claim remaining work, release lock, remove from registry).
	mgr.Cleanup(ctx)

	// Check if the owner's own work was abandoned (pushed to the loop but never
	// processed by OnAgentEvents). This can happen when the loop exits due to lock
	// loss, context cancellation, or other errors before the work item is consumed.
	// The work is still in "processing" state in Redis — requeue it so another
	// worker can pick it up.
	if mgr.Tracker().IsAbandoned(workID) {
		a.log(ctx, LogLevelWarn, "turn.owner_work_abandoned", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"work_id":    workID,
			"message":    "owner's work was not processed before loop exited, requeuing",
		})
		if reErr := a.queue.RequeueWork(context.Background(), workID); reErr != nil {
			a.log(ctx, LogLevelWarn, "turn.requeue_owner_failed", map[string]any{
				"work_id": workID,
				"error":   reErr.Error(),
			})
		}
	}

	a.log(ctx, LogLevelInfo, "turn.loop_exited", map[string]any{
		"session_id":  p.SessionID,
		"turn_id":     turnID,
		"work_kind":   string(p.Kind),
		"exit_reason": fmt.Sprintf("%v", exitState.ExitReason),
		"has_error":   exitState.ExitReason != nil,
	})

	exitReason := exitState.ExitReason
	turnDuration := time.Since(turnStart)

	if mgr.IsCancelledByQueue() {
		a.recordTurnEnd(ctx, span, p.SessionID, turnID, string(p.Kind), turnDuration, "cancel", nil)
		if err := a.cfg.CancelTurn(ctx, turnID, mgr.CancelReason()); err != nil {
			a.log(ctx, LogLevelError, "turn.cancel_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return nil
	}

	if errors.Is(innerCtx.Err(), context.Canceled) && ctx.Err() != nil {
		return ctx.Err()
	}

	switch {
	case exitReason == nil:
		a.log(ctx, LogLevelInfo, "turn.clean_exit", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "calling CompleteTurn",
		})
		a.recordTurnEnd(ctx, span, p.SessionID, turnID, string(p.Kind), turnDuration, "success", nil)
		if err := a.cfg.CompleteTurn(ctx, p.SessionID, turnID, mgr.LastMessage()); err != nil {
			a.log(ctx, LogLevelError, "turn.complete_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return nil

	case isInterruptError(exitReason):
		return a.handleInterruptExit(ctx, span, exitReason, p.SessionID, turnID)

	default:
		// Reactive compact: prompt-too-long retry logic.
		if IsPromptTooLongError(exitReason) && a.cfg.RecoverFromPromptTooLong != nil {
			if recovered := a.tryReactiveCompactRecovery(ctx, span, p, turnID, turnDuration, exitReason); recovered {
				return nil
			}
		}

		// Attempts exhausted or non-prompt-too-long: execute original FailTurn logic.
		a.recordTurnEnd(ctx, span, p.SessionID, turnID, string(p.Kind), turnDuration, "fail", exitReason)
		if err := a.cfg.FailTurn(ctx, turnID, exitReason); err != nil {
			a.log(ctx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		return exitReason
	}
}

// tryReactiveCompactRecovery attempts to recover from a prompt-too-long error
// by running compression and publishing a new work item. Returns true if
// recovery was successfully published (caller should return nil to let
// rtc-queue mark the current work complete). Returns false if the step
// failed or attempts are exhausted.
//
// The escalation strategy (L1→L2→L3) is driven by the ReactiveCompactAttempt
// field in WorkPayload: each recovery increments the attempt, and the next
// Process invocation reads it to pass the correct level to RecoverFromPromptTooLong.
func (a *Agent) tryReactiveCompactRecovery(
	ctx context.Context,
	span trace.Span,
	p WorkPayload,
	turnID string,
	turnDuration time.Duration,
	exitReason error,
) bool {
	// Escalate: use the attempt from the incoming payload + 1 so each
	// Process restart advances the compression level (L1 → L2 → L3).
	attempt := p.ReactiveCompactAttempt + 1
	if attempt > a.cfg.MaxReactiveCompactAttempts {
		a.log(ctx, LogLevelError, "turn.reactive_compact_attempts_exhausted", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"attempt":    attempt,
			"max":        a.cfg.MaxReactiveCompactAttempts,
		})
		return false
	}
	a.log(ctx, LogLevelWarn, "turn.prompt_too_long_recovering", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"attempt":    attempt,
		"max":        a.cfg.MaxReactiveCompactAttempts,
	})

	// Step 1: call reactive compact callback to compress context.
	if recoverErr := a.cfg.RecoverFromPromptTooLong(ctx, p.SessionID, attempt); recoverErr != nil {
		a.log(ctx, LogLevelError, "turn.reactive_compact_failed", map[string]any{
			"error": recoverErr.Error(),
		})
		return false
	}

	// Step 2: insert "compressing" feedback message.
	if a.cfg.InsertFeedbackMessage != nil {
		if err := a.cfg.InsertFeedbackMessage(ctx, p.SessionID, turnID,
			"context",
			"上下文超出限制",
			"对话内容太长，系统正在自动压缩后重试。请稍等片刻。",
			true, ""); err != nil {
			a.log(ctx, LogLevelWarn, "turn.insert_feedback_message_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
	}

	// Step 3: end current Turn (FailTurn must be before Publish).
	// Use WithSkipErrorMessage to prevent failTurn callback from
	// inserting a duplicate error message.
	a.recordTurnEnd(ctx, span, p.SessionID, turnID, string(p.Kind), turnDuration, "failed", fmt.Errorf("prompt_too_long_recovering"))
	skipCtx := WithSkipErrorMessage(ctx)
	if err := a.cfg.FailTurn(skipCtx, turnID, exitReason); err != nil {
		// NOTE: Unlike the normal FailTurn path, we log but do NOT propagate
		// this error. The reactive compact path has already compressed the
		// context and is about to publish a new work item; stopping here would
		// waste the compression work and leave the user stuck.
		a.log(ctx, LogLevelError, "turn.fail_callback_failed", map[string]any{
			"error": err.Error(),
		})
	}

	// Step 4: publish submit work item to trigger a new Process lifecycle.
	// Use context.Background() because ctx may be cancelled.
	payload := string(MarshalSubmitPayload(p.SessionID, attempt))
	if _, err := a.queue.Publish(context.Background(), p.SessionID, payload, 100); err != nil {
		a.log(ctx, LogLevelError, "turn.submit_publish_failed", map[string]any{
			"error":   err.Error(),
			"attempt": attempt,
		})
		return false
	}

	// Recovery published successfully.
	return true
}

// resolveTurnID obtains a turnID for the given work item based on its kind.
// For WorkKindSubmit it creates a new turn; for WorkKindResume it looks up the
// existing turn. Returns ErrNoActiveTurn (unwrapped) when a resume target is
// no longer active, so callers can distinguish transient from permanent errors.
func (a *Agent) resolveTurnID(ctx context.Context, sessionID, workID string, kind WorkKind) (string, error) {
	switch kind {
	case WorkKindSubmit:
		turnID, err := a.cfg.CreateTurn(ctx, sessionID, workID)
		if err != nil {
			return "", fmt.Errorf("CreateTurn: %w", err)
		}
		return turnID, nil
	case WorkKindResume:
		turnID, err := a.cfg.LookupTurn(ctx, sessionID, workID)
		if err != nil {
			return "", fmt.Errorf("LookupTurn: %w", err)
		}
		return turnID, nil
	default:
		return "", fmt.Errorf("unknown work kind: %q", kind)
	}
}

// beginTurn invokes the appropriate Begin/Resume callback when the caller is
// the owner of a new session manager (isNew=true). It records a tracing
// attribute and an error log on failure so the caller can simply propagate the
// returned error.
func (a *Agent) beginTurn(ctx context.Context, span trace.Span, sessionID, turnID string, kind WorkKind) error {
	switch kind {
	case WorkKindSubmit:
		if err := a.cfg.BeginTurn(ctx, turnID); err != nil {
			span.SetAttributes(attribute.String("turn.status", "error"))
			a.log(ctx, LogLevelError, "turn.begin_failed", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: BeginTurn: %w", err)
		}
	case WorkKindResume:
		if err := a.cfg.ResumeTurn(ctx, turnID); err != nil {
			span.SetAttributes(attribute.String("turn.status", "error"))
			a.log(ctx, LogLevelError, "turn.resume_failed", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: ResumeTurn: %w", err)
		}
	}
	return nil
}

// startCancelListener runs a goroutine that watches the queue's cancel channel
// and, on cancellation, stops the session turn loop and propagates the
// cancellation to both the inner work context and the turn-level context (which
// also cancels the in-flight LLM call running in mgr.Run).
func (a *Agent) startCancelListener(
	cancel <-chan rtcqueue.CancelMessage,
	mgr *SessionTurnManager,
	done <-chan struct{},
	innerCancel context.CancelFunc,
	turnCancel context.CancelFunc,
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				zap.L().Error("agent_process.cancel_listener_panic",
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())),
				)
			}
		}()
		select {
		case cm := <-cancel:
			mgr.SetCancelledByQueue(cm.Reason)
			if a.cfg.Cancel.GracePeriod > 0 {
				mgr.Loop().Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				mgr.Loop().Stop(adk.WithImmediate())
			}
			innerCancel()
			turnCancel()
		case <-done:
			return
		}
	}()
}
