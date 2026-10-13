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

	prepareCtx, prepareSpan := mgr.startSpanIfEnabled(ctx, "turn_agent.prepare_agent",
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
		"agent_type": fmt.Sprintf("%T", agent),
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
	eventsCtx, eventsSpan := mgr.startSpanIfEnabled(ctx, "turn_agent.on_agent_events",
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
	// Use a closure variable (not defer) so we can detect duplicate calls inside
	// the closure itself without adding another layer.
	completeWork := mgr.newCompleteWorkFunc(ctx, eventsSpan, tc, turnID)
	defer completeWork()

	// Idle warning watcher (warning-only, no exit).
	// AsyncIterator does not support Close()/Cancel(), so we only
	// log a warning when no events arrive for a prolonged period.
	activityReceived, idleWatcherDone := mgr.startIdleWatcher(ctx, eventsCtx, turnID)
	defer close(idleWatcherDone) // Ensure idle watcher exits when onAgentEvents returns

	// Consume events until the iterator is exhausted, interrupted, or fails.
	eventCount := 0
	for {
		mgr.log(eventsCtx, LogLevelDebug, "on_agent_events.waiting_next", map[string]any{
			"session_id":  mgr.sessionID,
			"turn_id":     turnID,
			"context_err": ctx.Err(),
		})

		ev, ok := mgr.consumeOneEvent(events, activityReceived)
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

		if err := mgr.dispatchEvents(eventsCtx, turnID, ev); err != nil {
			return mgr.handleDispatchError(eventsSpan, eventsCtx, turnID, err, eventCount)
		}
	}
}

// newCompleteWorkFunc returns a closure that completes all work items tracked
// in tc.Consumed. The closure is idempotent - calling it more than once is a no-op.
func (mgr *SessionTurnManager) newCompleteWorkFunc(
	parentCtx context.Context,
	span trace.Span,
	tc *adk.TurnContext[TurnWorkItem, *schema.Message],
	turnID string,
) func() {
	called := false
	return func() {
		if called {
			return
		}
		called = true
		span.AddEvent("complete_work")
		// Use a timeout context derived from the parent context to ensure CompleteWork
		// finishes before the request context is fully cancelled. This prevents state
		// inconsistency where the work is marked complete in the tracker but not in the queue.
		// If the parent context is already cancelled, fall back to a background context with
		// a short timeout to ensure cleanup still happens.
		var completeCtx context.Context
		var completeCancel context.CancelFunc
		if parentCtx.Err() != nil {
			// Parent context already cancelled, use background with short timeout.
			completeCtx, completeCancel = context.WithTimeout(context.Background(), 5*time.Second)
		} else {
			// Parent context still valid, use it with a reasonable timeout.
			completeCtx, completeCancel = context.WithTimeout(parentCtx, 10*time.Second)
		}
		defer completeCancel()
		for _, item := range tc.Consumed {
			if item.WorkID == "" {
				continue
			}
			if err := mgr.queue.CompleteWork(completeCtx, item.WorkID); err != nil {
				span.RecordError(err)
				mgr.log(completeCtx, LogLevelError, "on_agent_events.complete_work_failed", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"work_id":    item.WorkID,
					"error":      err.Error(),
				})
			} else {
				mgr.log(completeCtx, LogLevelDebug, "on_agent_events.complete_work_success", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"work_id":    item.WorkID,
				})
			}
			mgr.tracker.Complete(item.WorkID)
		}
	}
}

// startIdleWatcher launches a background goroutine that warns (but does not exit)
// when no events arrive for eventIdleWarningTimeout.
// Returns:
//   - activityReceived: a channel the event loop must signal on each received event
//   - idleWatcherDone:  a channel that must be closed when the event loop returns
func (mgr *SessionTurnManager) startIdleWatcher(parentCtx, eventsCtx context.Context, turnID string) (chan struct{}, chan struct{}) {
	activityReceived := make(chan struct{}, 1)
	idleWatcherDone := make(chan struct{})
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
			case <-parentCtx.Done():
				return
			case <-idleWatcherDone:
				return // Explicit exit signal when onAgentEvents returns
			case <-activityReceived:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(eventIdleWarningTimeout)
			case <-timer.C:
				mgr.log(eventsCtx, LogLevelWarn, "on_agent_events.idle_warning", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"timeout":    eventIdleWarningTimeout.String(),
					"action":     "warning_only_no_exit",
				})
			}
		}
	}()
	return activityReceived, idleWatcherDone
}

// consumeOneEvent pulls the next event from the iterator and signals the idle watcher.
// Returns (event, true) when an event is available, or (nil, false) when the iterator is exhausted.
func (mgr *SessionTurnManager) consumeOneEvent(
	events *adk.AsyncIterator[*adk.AgentEvent],
	activityReceived chan<- struct{},
) (*adk.AgentEvent, bool) {
	ev, ok := events.Next()
	if !ok {
		return nil, false
	}
	// Signal the idle watcher that we received an event.
	select {
	case activityReceived <- struct{}{}:
	default:
	}
	return ev, true
}

// handleDispatchError translates a dispatch failure into the appropriate return value.
// For interrupt errors the original *adk.InterruptError is returned unchanged; for all
// other errors a wrapped error is returned so the caller's event loop terminates.
func (mgr *SessionTurnManager) handleDispatchError(
	eventsSpan trace.Span,
	eventsCtx context.Context,
	turnID string,
	err error,
	eventCount int,
) error {
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
