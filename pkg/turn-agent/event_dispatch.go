package turnagent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

	prepareCtx, prepareSpan := mgr.startSpanIfEnabled(ctx, "prepare_agent",
		trace.WithAttributes(
			attribute.String("session.id", mgr.sessionID),
			attribute.String("turn.id", turnID),
		),
	)
	defer prepareSpan.End()

	mgr.log(prepareCtx, LogLevelDebug, "prepare_agent.start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
	})

	prepareSpan.AddEvent("create_tools")
	tools, err := mgr.cfg.CreateTools(prepareCtx, mgr.sessionID, turnID)
	if err != nil {
		prepareSpan.RecordError(err)
		prepareSpan.SetAttributes(attribute.String("prepare.status", "tools_failed"))
		mgr.log(prepareCtx, LogLevelError, "prepare_agent.create_tools_failed", map[string]any{
			"session_id": mgr.sessionID,
			"turn_id":    turnID,
			"error":      err.Error(),
		})
		return nil, fmt.Errorf("turnagent: CreateTools: %w", err)
	}
	prepareSpan.SetAttributes(attribute.Int("tools.count", len(tools)))

	prepareSpan.AddEvent("create_agent")
	agent, err := mgr.cfg.CreateAgent(prepareCtx, mgr.sessionID, turnID, tools)
	if err != nil {
		prepareSpan.RecordError(err)
		prepareSpan.SetAttributes(attribute.String("prepare.status", "agent_failed"))
		mgr.log(prepareCtx, LogLevelError, "prepare_agent.create_agent_failed", map[string]any{
			"session_id": mgr.sessionID,
			"turn_id":    turnID,
			"error":      err.Error(),
		})
		return nil, fmt.Errorf("turnagent: CreateAgent: %w", err)
	}

	prepareSpan.SetAttributes(attribute.String("prepare.status", "success"))
	mgr.log(prepareCtx, LogLevelInfo, "prepare_agent.done", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
		"tools":      len(tools),
	})
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
	eventsCtx, eventsSpan := mgr.startSpanIfEnabled(ctx, "on_agent_events",
		trace.WithAttributes(
			attribute.String("session.id", mgr.sessionID),
			attribute.String("turn.id", turnID),
			attribute.Int("consumed.count", len(tc.Consumed)),
		),
	)
	defer eventsSpan.End()

	mgr.log(eventsCtx, LogLevelInfo, "on_agent_events.start", map[string]any{
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
		eventsSpan.AddEvent("complete_work")
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bgCancel()
		for _, item := range tc.Consumed {
			if item.WorkID == "" {
				continue
			}
			if err := mgr.queue.CompleteWork(bgCtx, item.WorkID); err != nil {
				eventsSpan.RecordError(err)
				mgr.log(bgCtx, LogLevelError, "on_agent_events.complete_work_failed", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"work_id":    item.WorkID,
					"error":      err.Error(),
				})
			} else {
				mgr.log(bgCtx, LogLevelDebug, "on_agent_events.complete_work_success", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"work_id":    item.WorkID,
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
					"stack":      string(debug.Stack()),
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

	eventCount := 0
	for {
		ctxErr := ctx.Err()
		mgr.log(eventsCtx, LogLevelDebug, "on_agent_events.waiting_next", map[string]any{
			"session_id":  mgr.sessionID,
			"turn_id":     turnID,
			"context_err": ctxErr,
		})
		ev, ok := events.Next()
		if !ok {
			eventsSpan.SetAttributes(
				attribute.Int("events.processed", eventCount),
				attribute.String("events.status", "completed"),
			)
			mgr.log(eventsCtx, LogLevelInfo, "on_agent_events.done", map[string]any{
				"session_id":  mgr.sessionID,
				"turn_id":     turnID,
				"event_count": eventCount,
			})
			return nil
		}
		eventCount++
		// Signal the idle watcher that we received an event.
		select {
		case activityReceived <- struct{}{}:
		default:
		}
		if err := mgr.dispatchEvents(eventsCtx, turnID, ev); err != nil {
			var interruptErr *adk.InterruptError
			if errors.As(err, &interruptErr) {
				eventsSpan.SetAttributes(
					attribute.Int("events.processed", eventCount),
					attribute.String("events.status", "interrupted"),
				)
				mgr.log(eventsCtx, LogLevelInfo, "on_agent_events.interrupted", map[string]any{
					"session_id":   mgr.sessionID,
					"turn_id":      turnID,
					"num_contexts": len(interruptErr.InterruptContexts),
					"event_count":  eventCount,
				})
				return err
			}
			eventsSpan.RecordError(err)
			eventsSpan.SetAttributes(
				attribute.Int("events.processed", eventCount),
				attribute.String("events.status", "error"),
			)
			mgr.log(eventsCtx, LogLevelError, "on_agent_events.dispatch_error", map[string]any{
				"session_id":  mgr.sessionID,
				"turn_id":     turnID,
				"error":       err.Error(),
				"event_count": eventCount,
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
