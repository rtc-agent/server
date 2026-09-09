package turnagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// Agent is a stateful processor that manages turns for sessions.
// Each Process call creates a per-turn TurnAgent that claims the session lock,
// runs the eino TurnLoop, and releases resources when the turn ends.
type Agent struct {
	cfg      Config
	queue    *rtcqueue.Queue
	workerID string
}

// New constructs an Agent.
func New(cfg Config, queue *rtcqueue.Queue, workerID string) (*Agent, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("turnagent: %w", err)
	}
	if cfg.DeriveCheckpointID == nil {
		cfg.DeriveCheckpointID = func(sessionID string) string {
			return "turnagent:session:" + sessionID
		}
	}
	a := &Agent{
		cfg:      cfg,
		queue:    queue,
		workerID: workerID,
	}
	a.logIfEnabled(context.Background(), LogLevelDebug, "agent.new", map[string]any{
		"has_logger":         cfg.Logger != nil,
		"has_tracer":         cfg.Tracer != nil,
		"has_metrics":        cfg.Metrics != nil,
		"enable_llm_logging": cfg.EnableLLMLogging,
		"has_callbacks":      len(cfg.Callbacks) > 0,
	})
	return a, nil
}

// buildEinoConfig constructs the eino TurnLoopConfig for a turn.
// sessionID and checkpointID are fixed for the session.
// turnID is passed via TurnWorkItem for each turn, not captured in closures.
// lastMessagePtr is a pointer to a *Message that will be updated during the turn
// to track the last assistant message (for Sub Agent support).
// onTurnComplete is called when a turn completes (OnAgentEvents returns).
func (a *Agent) buildEinoConfig(sessionID, checkpointID string, lastMessagePtr **Message, onTurnComplete func()) adk.TurnLoopConfig[TurnWorkItem, *schema.Message] {
	return adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], items []TurnWorkItem) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
			if len(items) == 0 {
				return nil, fmt.Errorf("turnagent: GenInput called with no items")
			}
			// Extract turnID from the first item
			turnID := items[0].TurnID

			a.logIfEnabled(ctx, LogLevelDebug, "gen_input.start", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"item_count": len(items),
			})

			ctx = WithSessionID(ctx, sessionID)
			ctx = WithTurnID(ctx, turnID)

			if len(a.cfg.Callbacks) > 0 {
				ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, a.cfg.Callbacks...)
			}

			msgs, err := a.cfg.LoadMessages(ctx, sessionID)
			if err != nil {
				a.logIfEnabled(ctx, LogLevelError, "gen_input.load_messages_failed", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
				})
				return nil, fmt.Errorf("turnagent: LoadMessages: %w", err)
			}
			a.logIfEnabled(ctx, LogLevelDebug, "gen_input.messages_loaded", map[string]any{
				"session_id":    sessionID,
				"turn_id":       turnID,
				"message_count": len(msgs),
			})
			if len(msgs) == 0 {
				a.logIfEnabled(ctx, LogLevelWarn, "turn.empty_messages", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
				})
			}
			return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
				RunCtx: ctx,
				Input: &adk.TypedAgentInput[*schema.Message]{
					Messages:        toEinoMessages(msgs),
					EnableStreaming: true,
				},
				Consumed: items,
			}, nil
		},

		GenResume: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], interrupted, unhandled, newItems []TurnWorkItem) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
			var turnID string
			if len(newItems) > 0 {
				turnID = newItems[0].TurnID
			} else if len(interrupted) > 0 {
				turnID = interrupted[0].TurnID
			} else if len(unhandled) > 0 {
				turnID = unhandled[0].TurnID
			}

			ctx = WithSessionID(ctx, sessionID)
			if turnID != "" {
				ctx = WithTurnID(ctx, turnID)
			}

			if len(a.cfg.Callbacks) > 0 {
				ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, a.cfg.Callbacks...)
			}

			allItems := make([]TurnWorkItem, 0, len(interrupted)+len(unhandled)+len(newItems))
			allItems = append(allItems, interrupted...)
			allItems = append(allItems, unhandled...)
			allItems = append(allItems, newItems...)

			return &adk.GenResumeResult[TurnWorkItem, *schema.Message]{
				RunCtx:   ctx,
				Consumed: allItems,
			}, nil
		},

		PrepareAgent: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], consumed []TurnWorkItem) (adk.Agent, error) {
			var turnID string
			if len(consumed) > 0 {
				turnID = consumed[0].TurnID
			}
			// Fallback: get turnID from context (set by GenInput/GenResume)
			if turnID == "" {
				turnID = TurnIDFromContext(ctx)
			}

			a.logIfEnabled(ctx, LogLevelDebug, "prepare_agent.start", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
			})

			tools, err := a.cfg.CreateTools(ctx, sessionID, turnID)
			if err != nil {
				a.logIfEnabled(ctx, LogLevelError, "prepare_agent.create_tools_failed", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
				})
				return nil, fmt.Errorf("turnagent: CreateTools: %w", err)
			}
			a.logIfEnabled(ctx, LogLevelDebug, "prepare_agent.tools_created", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"tool_count": len(tools),
			})

			agent, err := a.cfg.CreateAgent(ctx, sessionID, turnID, tools)
			if err != nil {
				a.logIfEnabled(ctx, LogLevelError, "prepare_agent.create_agent_failed", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
				})
				return nil, fmt.Errorf("turnagent: CreateAgent: %w", err)
			}
			a.logIfEnabled(ctx, LogLevelDebug, "prepare_agent.done", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
			})
			return agent, nil
		},

		OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			// Extract turnID from context
			turnID := TurnIDFromContext(ctx)
			a.logIfEnabled(ctx, LogLevelInfo, "on_agent_events.start", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
			})

			for {
				// Check context status before waiting
				ctxErr := ctx.Err()
				a.logIfEnabled(ctx, LogLevelDebug, "on_agent_events.waiting_next", map[string]any{
					"session_id":  sessionID,
					"turn_id":     turnID,
					"context_err": ctxErr,
				})
				ev, ok := events.Next()
				if !ok {
					a.logIfEnabled(ctx, LogLevelInfo, "on_agent_events.done", map[string]any{
						"session_id": sessionID,
						"turn_id":    turnID,
					})
					// Agent finished producing events.
					// Signal turn completion so Process() can check for next work.
					// TurnLoop will return to buffer.Receive() and block waiting for new items.
					// Process() will atomically check the queue for next work.
					// If more work exists, Process() pushes it to buffer; otherwise calls Stop().
					if onTurnComplete != nil {
						onTurnComplete()
					}
					return nil
				}
				if err := a.dispatchEvents(ctx, sessionID, turnID, ev, lastMessagePtr); err != nil {
					// InterruptError is a legitimate business pause, not an error.
					// Log it at INFO level to avoid misleading error logs.
					var interruptErr *adk.InterruptError
					if errors.As(err, &interruptErr) {
						a.logIfEnabled(ctx, LogLevelInfo, "on_agent_events.interrupted", map[string]any{
							"session_id":   sessionID,
							"turn_id":      turnID,
							"num_contexts": len(interruptErr.InterruptContexts),
						})
						return err
					}
					a.logIfEnabled(ctx, LogLevelError, "on_agent_events.dispatch_error", map[string]any{
						"session_id": sessionID,
						"turn_id":    turnID,
						"error":      err.Error(),
					})
					return fmt.Errorf("turnagent: PublishEvent: %w", err)
				}
			}
		},

		Store:        a.cfg.CheckpointStore,
		CheckpointID: checkpointID,
	}
}

// dispatchEvents translates one eino AgentEvent into one or more flattened
// pkg Event calls to the application's PublishEvent callback.
//
// For streaming events, the pkg fully consumes the underlying stream, emitting
// EventKindStreamChunk per Recv() and EventKindStreamEnd at EOF. The upper
// application never touches the stream object itself.
//
// The stream consumption loop respects ctx cancellation: if ctx is cancelled
// while Recv() is blocking, the stream is closed and the loop exits cleanly.
//
// CancelError is intentionally swallowed: it is eino's internal cancellation
// signal, not an application-visible error. The turn's cancellation is already
// handled by the lifecycle path in Process() (via the cancel channel and the
// cancelledByQueue flag).
//
// lastMessagePtr is used to store the last assistant message for Sub Agent support.
func (a *Agent) dispatchEvents(ctx context.Context, sessionID, turnID string, ev *adk.AgentEvent, lastMessagePtr **Message) error {
	// 1. Event-level error.
	if ev.Err != nil {
		// Swallow CancelError — it's eino's internal cancellation signal.
		// The turn-level cancel path in Process() handles the lifecycle
		// transition; propagating CancelError here would cause eino's
		// TurnLoop to terminate unexpectedly.
		var cancelErr *adk.CancelError
		if errors.As(ev.Err, &cancelErr) {
			return nil
		}
		a.logIfEnabled(ctx, LogLevelError, "event.error", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"agent_name": ev.AgentName,
			"error":      ev.Err.Error(),
		})
		return a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
			Kind:      EventKindError,
			AgentName: ev.AgentName,
			Err:       ev.Err,
		})
	}

		// 2. No output — skip.
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			// Check for interrupt action — return InterruptError to signal turn loop
			if ev.Action != nil && ev.Action.Interrupted != nil {
				return &adk.InterruptError{
					InterruptContexts: ev.Action.Interrupted.InterruptContexts,
				}
			}
			return nil
		}
		mv := ev.Output.MessageOutput

	// 3. Streaming: consume the stream, emit chunks + end.
	if mv.IsStreaming {
		return a.consumeStream(ctx, sessionID, turnID, ev.AgentName, string(mv.Role), mv.ToolName, mv.MessageStream, lastMessagePtr)
	}

	// 4. Non-streaming: emit one message event.
	var tokenUsage *TokenUsage
	if mv.Message != nil && mv.Message.ResponseMeta != nil && mv.Message.ResponseMeta.Usage != nil {
		tokenUsage = extractTokenUsage(mv.Message.ResponseMeta.Usage)
	}
	// Track the last assistant message for Sub Agent support.
	if lastMessagePtr != nil && mv.Role == schema.Assistant && mv.Message != nil {
		*lastMessagePtr = fromEinoMessage(mv.Message)
	}
	return a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
		Kind:       EventKindMessage,
		AgentName:  ev.AgentName,
		Role:       string(mv.Role),
		ToolName:   mv.ToolName,
		Message:    fromEinoMessage(mv.Message),
		TokenUsage: tokenUsage,
	})
}

// consumeStream drives a stream reader to completion, translating each
// received chunk into an EventKindStreamChunk callback and emitting a final
// EventKindStreamEnd on EOF.
//
// The loop uses a goroutine + select pattern so that ctx cancellation unblocks
// stream.Recv() — closing the stream on the way out so eino's resources are
// released.
//
// sessionLoop is used to store the last assistant message for Sub Agent support.
func (a *Agent) consumeStream(ctx context.Context, sessionID, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message], lastMessagePtr **Message) error {
	// No `defer stream.Close()` here: we close explicitly on each exit path
	// below. A deferred close would fire on top of the explicit close on the
	// ctx.Done / EOF / error paths, causing a double close.

	a.logIfEnabled(ctx, LogLevelDebug, "stream.consume_start", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
		"agent_name": agentName,
		"role":       role,
	})

	// Aggregate token usage across all chunks by taking the max of each field.
	// This ensures that even when intermediate ChatModel calls in ReAct loops
	// don't carry Usage on their final chunk, we still capture the usage data
	// (which may appear on any chunk from the model provider).
	var maxUsage *schema.TokenUsage
	updateMaxUsage := func(usage *schema.TokenUsage) {
		if usage == nil {
			return
		}
		if maxUsage == nil {
			maxUsage = &schema.TokenUsage{}
		}
		if usage.PromptTokens > maxUsage.PromptTokens {
			maxUsage.PromptTokens = usage.PromptTokens
		}
		if usage.CompletionTokens > maxUsage.CompletionTokens {
			maxUsage.CompletionTokens = usage.CompletionTokens
		}
		if usage.TotalTokens > maxUsage.TotalTokens {
			maxUsage.TotalTokens = usage.TotalTokens
		}
		if usage.PromptTokenDetails.CachedTokens > maxUsage.PromptTokenDetails.CachedTokens {
			maxUsage.PromptTokenDetails.CachedTokens = usage.PromptTokenDetails.CachedTokens
		}
		if usage.CompletionTokensDetails.ReasoningTokens > maxUsage.CompletionTokensDetails.ReasoningTokens {
			maxUsage.CompletionTokensDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
		}
	}

	type recvResult struct {
		msg *schema.Message
		err error
	}

	// Aggregate content across all chunks for Sub Agent support.
	// Only used when role is "assistant" and lastMessage is non-nil.
	var contentBuilder strings.Builder

	for {
		// Recv in a goroutine so we can race it against ctx cancellation.
		ch := make(chan recvResult, 1)
		go func() {
			msg, err := stream.Recv()
			ch <- recvResult{msg, err}
		}()

		select {
		case <-ctx.Done():
			// ctx cancelled (either admin cancel or worker shutdown). Close the
			// stream and propagate. No EventKindStreamEnd is emitted — the turn
			// is being torn down via the cancel/fail path in Process().
			a.logIfEnabled(ctx, LogLevelInfo, "stream.ctx_cancelled", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
			})
			stream.Close()
			return ctx.Err()

		case res := <-ch:
			if errors.Is(res.err, io.EOF) {
				// Stream complete. Close the underlying reader before emitting
				// the terminal event, so eino releases its resources before
				// the application processes the end marker.
				a.logIfEnabled(ctx, LogLevelDebug, "stream.eof", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
				})
				stream.Close()
				// Extract aggregated token usage (max of all chunks) and pass it
				// on StreamEnd. This ensures that even when the final chunk didn't
				// carry Usage (common for intermediate ChatModel calls in ReAct
				// loops), the application can still persist the token data.
				var aggregatedTokenUsage *TokenUsage
				if maxUsage != nil {
					aggregatedTokenUsage = extractTokenUsage(maxUsage)
					a.logIfEnabled(ctx, LogLevelInfo, "stream.eof.aggregated_usage", map[string]any{
						"session_id":     sessionID,
						"turn_id":        turnID,
						"agent_name":     agentName,
						"role":           role,
						"prompt_tokens":  maxUsage.PromptTokens,
						"completion_tokens": maxUsage.CompletionTokens,
						"total_tokens":   maxUsage.TotalTokens,
						"cached_tokens":  maxUsage.PromptTokenDetails.CachedTokens,
						"reasoning_tokens": maxUsage.CompletionTokensDetails.ReasoningTokens,
					})
				} else {
					a.logIfEnabled(ctx, LogLevelWarn, "stream.eof.no_usage", map[string]any{
						"session_id": sessionID,
						"turn_id":    turnID,
						"agent_name": agentName,
						"role":       role,
						"message":    "maxUsage is nil after consuming all chunks",
					})
				}
				// Track the last assistant message for Sub Agent support.
				if lastMessagePtr != nil && role == string(schema.Assistant) && contentBuilder.Len() > 0 {
					*lastMessagePtr = &Message{
						Role:    role,
						Content: contentBuilder.String(),
					}
				}
				return a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
					Kind:       EventKindStreamEnd,
					AgentName:  agentName,
					Role:       role,
					ToolName:   toolName,
					TokenUsage: aggregatedTokenUsage,
				})
			}
			if res.err != nil {
				// Swallow CancelError — same reason as in dispatchEvents.
				var cancelErr *adk.CancelError
				if errors.As(res.err, &cancelErr) {
					stream.Close()
					return nil
				}
				// Transport error mid-stream. Surface as an event error; the
				// turn's final disposition is decided by the caller based on
				// how the TurnLoop exits.
				stream.Close()
				if pubErr := a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
					Kind:      EventKindError,
					AgentName: agentName,
					Err:       res.err,
				}); pubErr != nil {
					return pubErr
				}
				return res.err
			}

			// Valid chunk. Translate and forward.
			var finishReason string
			var tokenUsage *TokenUsage
			if res.msg.ResponseMeta != nil {
				finishReason = res.msg.ResponseMeta.FinishReason
				// Log ResponseMeta presence for debugging.
				a.logIfEnabled(ctx, LogLevelDebug, "stream.chunk.response_meta", map[string]any{
					"session_id":     sessionID,
					"turn_id":        turnID,
					"agent_name":     agentName,
					"role":           role,
					"has_usage":      res.msg.ResponseMeta.Usage != nil,
					"finish_reason":  finishReason,
					"usage_details": func() string {
						if res.msg.ResponseMeta.Usage == nil {
							return "nil"
						}
						u := res.msg.ResponseMeta.Usage
						return fmt.Sprintf("prompt=%d completion=%d total=%d cached=%d reasoning=%d",
							u.PromptTokens, u.CompletionTokens, u.TotalTokens,
							u.PromptTokenDetails.CachedTokens, u.CompletionTokensDetails.ReasoningTokens)
					}(),
				})
				// Accumulate usage (take max of each field across all chunks).
				updateMaxUsage(res.msg.ResponseMeta.Usage)
				if res.msg.ResponseMeta.Usage != nil {
					tokenUsage = extractTokenUsage(res.msg.ResponseMeta.Usage)
				}
			}
			a.logIfEnabled(ctx, LogLevelDebug, "stream.chunk", map[string]any{
				"session_id":    sessionID,
				"turn_id":       turnID,
				"agent_name":    agentName,
				"role":          role,
				"finish_reason": finishReason,
				"has_content":   res.msg.Content != "",
				"is_final":      finishReason != "",
			})
			// Aggregate content for Sub Agent support.
			if res.msg.Content != "" {
				contentBuilder.WriteString(res.msg.Content)
			}
			if err := a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
				Kind:             EventKindStreamChunk,
				AgentName:        agentName,
				Role:             role,
				ToolName:         toolName,
				Content:          res.msg.Content,
				ReasoningContent: res.msg.ReasoningContent,
				FinishReason:     finishReason,
				TokenUsage:       tokenUsage,
			}); err != nil {
				// PublishEvent error: close the stream before propagating so
				// eino resources are released.
				stream.Close()
				return err
			}
		}
	}
}

// =============================================================================
// Helpers
// =============================================================================

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
