package turnagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// eventIdleWarningTimeout is the duration after which an idle warning is
// logged if no events are received from the AsyncIterator.
//
// 10 minutes is chosen as a conservative threshold: tool executions (especially
// file operations, searches, or shell commands) can legitimately take several
// minutes. A shorter timeout would produce false-positive warnings that clutter
// logs. The warning is informational only — AsyncIterator does not support
// Close()/Cancel(), so we cannot exit the loop on idle (plan C: warn-only).
const eventIdleWarningTimeout = 10 * time.Minute

// prepareAgent creates the eino Agent with tools for the current turn.
func (mgr *SessionTurnManager) prepareAgent(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	consumed []TurnWorkItem,
) (adk.Agent, error) {
	var turnID string
	if len(consumed) > 0 {
		turnID = consumed[0].TurnID
	}
	if turnID == "" {
		turnID = TurnIDFromContext(ctx)
	}

	mgr.log(ctx, LogLevelDebug, "prepare_agent.start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
	})

	tools, err := mgr.cfg.CreateTools(ctx, mgr.sessionID, turnID)
	if err != nil {
		return nil, fmt.Errorf("turnagent: CreateTools: %w", err)
	}

	agent, err := mgr.cfg.CreateAgent(ctx, mgr.sessionID, turnID, tools)
	if err != nil {
		return nil, fmt.Errorf("turnagent: CreateAgent: %w", err)
	}
	return agent, nil
}

// onAgentEvents consumes agent events, dispatching them to the appropriate
// handlers (stream, message, interrupt, error).
func (mgr *SessionTurnManager) onAgentEvents(
	ctx context.Context,
	tc *adk.TurnContext[TurnWorkItem, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent],
) error {
	turnID := TurnIDFromContext(ctx)
	mgr.log(ctx, LogLevelInfo, "on_agent_events.start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
	})

	// Ensure CompleteWork is called even if we return early due to interrupt/error.
	completeWorkCalled := false
	completeWork := func() {
		if completeWorkCalled {
			return
		}
		completeWorkCalled = true
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bgCancel()
		for _, item := range tc.Consumed {
			if item.WorkID == "" {
				continue
			}
			if err := mgr.queue.CompleteWork(bgCtx, item.WorkID); err != nil {
				mgr.log(bgCtx, LogLevelError, "on_agent_events.complete_work_failed", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"work_id":    item.WorkID,
					"error":      err.Error(),
				})
			}
			mgr.tracker.Complete(item.WorkID)
		}
	}
	defer completeWork()

	// Idle warning watcher (warning-only, no exit).
	// AsyncIterator does not support Close()/Cancel(), so we only
	// log a warning when no events arrive for a prolonged period.
	activityReceived := make(chan struct{}, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				mgr.log(context.Background(), LogLevelError, "on_agent_events.idle_watcher_panic", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"panic":      fmt.Sprintf("%v", r),
				})
			}
		}()
		timer := time.NewTimer(eventIdleWarningTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activityReceived:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(eventIdleWarningTimeout)
			case <-timer.C:
				mgr.log(ctx, LogLevelWarn, "on_agent_events.idle_warning", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"timeout":    eventIdleWarningTimeout.String(),
					"action":     "warning_only_no_exit",
				})
			}
		}
	}()

	for {
		ctxErr := ctx.Err()
		mgr.log(ctx, LogLevelDebug, "on_agent_events.waiting_next", map[string]any{
			"session_id":  mgr.sessionID,
			"turn_id":     turnID,
			"context_err": ctxErr,
		})
		ev, ok := events.Next()
		if !ok {
			mgr.log(ctx, LogLevelInfo, "on_agent_events.done", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
			})
			return nil
		}
		// Signal the idle watcher that we received an event.
		select {
		case activityReceived <- struct{}{}:
		default:
		}
		if err := mgr.dispatchEvents(ctx, turnID, ev); err != nil {
			var interruptErr *adk.InterruptError
			if errors.As(err, &interruptErr) {
				mgr.log(ctx, LogLevelInfo, "on_agent_events.interrupted", map[string]any{
					"session_id":   mgr.sessionID,
					"turn_id":      turnID,
					"num_contexts": len(interruptErr.InterruptContexts),
				})
				return err
			}
			mgr.log(ctx, LogLevelError, "on_agent_events.dispatch_error", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: PublishEvent: %w", err)
		}
	}
}

// dispatchEvents translates one eino AgentEvent into flattened Event calls.
func (mgr *SessionTurnManager) dispatchEvents(ctx context.Context, turnID string, ev *adk.AgentEvent) error {
	if ev.Err != nil {
		var cancelErr *adk.CancelError
		if errors.As(ev.Err, &cancelErr) {
			return nil
		}
		mgr.log(ctx, LogLevelError, "event.error", map[string]any{
			"session_id": mgr.sessionID,
			"turn_id":    turnID,
			"agent_name": ev.AgentName,
			"error":      ev.Err.Error(),
		})
		return mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
			Kind:      EventKindError,
			AgentName: ev.AgentName,
			Err:       ev.Err,
		})
	}

	if ev.Output == nil || ev.Output.MessageOutput == nil {
		if ev.Action != nil && ev.Action.Interrupted != nil {
			return &adk.InterruptError{
				InterruptContexts: ev.Action.Interrupted.InterruptContexts,
			}
		}
		return nil
	}
	mv := ev.Output.MessageOutput

	if mv.IsStreaming {
		return mgr.consumeStream(ctx, turnID, ev.AgentName, string(mv.Role), mv.ToolName, mv.MessageStream)
	}

	var tokenUsage *TokenUsage
	if mv.Message != nil && mv.Message.ResponseMeta != nil && mv.Message.ResponseMeta.Usage != nil {
		tokenUsage = extractTokenUsage(mv.Message.ResponseMeta.Usage)
	}
	if mv.Role == schema.Assistant && mv.Message != nil {
		mgr.setLastMessage(fromEinoMessage(mv.Message))
	}
	return mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
		Kind:       EventKindMessage,
		AgentName:  ev.AgentName,
		Role:       string(mv.Role),
		ToolName:   mv.ToolName,
		Message:    fromEinoMessage(mv.Message),
		TokenUsage: tokenUsage,
	})
}
