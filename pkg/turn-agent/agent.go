package turnagent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
)

// Agent is a stateless processor that executes one turn per rtc-queue Work
// item. All persistent state lives in the eino checkpoint store and the
// application's database.
//
// An Agent is safe to share across goroutines and may be used as the OnWork
// callback of one or more rtcqueue.Worker instances.
type Agent struct {
	cfg Config
}

// New constructs an Agent. Returns an error if required Config fields are
// missing.
func New(cfg Config) (*Agent, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("turnagent: %w", err)
	}
	if cfg.DeriveCheckpointID == nil {
		cfg.DeriveCheckpointID = func(sessionID string) string {
			return "turnagent:session:" + sessionID
		}
	}
	a := &Agent{cfg: cfg}
	a.logIfEnabled(context.Background(), LogLevelDebug, "agent.new", map[string]any{
		"has_logger":         cfg.Logger != nil,
		"has_tracer":         cfg.Tracer != nil,
		"has_metrics":        cfg.Metrics != nil,
		"enable_llm_logging": cfg.EnableLLMLogging,
		"has_callbacks":      len(cfg.Callbacks) > 0,
	})
	return a, nil
}

// buildEinoConfig constructs the eino TurnLoopConfig from the application's
// callbacks. sessionID and turnID are captured in closures so GenInput,
// GenResume, PrepareAgent, and OnAgentEvents can forward them to the data
// callbacks without carrying them in the eino item type.
func (a *Agent) buildEinoConfig(sessionID, turnID, checkpointID string) adk.TurnLoopConfig[WorkPayload, *schema.Message] {
	return adk.TurnLoopConfig[WorkPayload, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[WorkPayload, *schema.Message], items []WorkPayload) (*adk.GenInputResult[WorkPayload, *schema.Message], error) {
			// Inject sessionID/turnID into ctx BEFORE callbacks, so eino
			// callback handlers (metrics, logging) can read them via
			// SessionIDFromContext / TurnIDFromContext on every ChatModel call.
			// This is what makes the callback handler able to label per-call
			// records with the session/turn that produced them.
			ctx = WithSessionID(ctx, sessionID)
			ctx = WithTurnID(ctx, turnID)

			// Inject eino callbacks into ctx so downstream model/tool calls
			// pick up application-level tracing / metrics handlers. The empty
			// RunInfo mirrors turn-loop's session-level injection — per-component
			// RunInfo is filled in by eino as it walks the agent tree.
			if len(a.cfg.Callbacks) > 0 {
				ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, a.cfg.Callbacks...)
			}

			msgs, err := a.cfg.LoadMessages(ctx, sessionID)
			if err != nil {
				return nil, fmt.Errorf("turnagent: LoadMessages: %w", err)
			}
			if len(msgs) == 0 {
				// The implementation is responsible for always returning a
				// meaningful history (system prompt + user message at minimum).
				// An empty slice almost always indicates a bug upstream —
				// log it so the issue surfaces in logs, but still run the
				// agent with empty input so we don't mask the underlying
				// state by failing the turn.
				a.logIfEnabled(ctx, LogLevelWarn, "turn.empty_messages", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
				})
			}
			return &adk.GenInputResult[WorkPayload, *schema.Message]{
				RunCtx: ctx,
				Input: &adk.TypedAgentInput[*schema.Message]{
					Messages:        toEinoMessages(msgs),
					EnableStreaming: true,
				},
				Consumed: items,
			}, nil
		},

		GenResume: func(ctx context.Context, loop *adk.TurnLoop[WorkPayload, *schema.Message], interrupted, unhandled, newItems []WorkPayload) (*adk.GenResumeResult[WorkPayload, *schema.Message], error) {
			// RTC 模式下：不需要向 ResumeParams 里塞任何数据。工具在重新进入后
			// 通过 tool.GetInterruptState 拿到 RtcID，自己查 DB 取结果。
			//
			// 若未来引入非 RTC 的中断类型（例如简单的确认类问答）且需要把 answer
			// 透传给工具，可在此处添加一个 LoadResumeData 回调，把结果塞到
			// ResumeParams.Targets。
			return &adk.GenResumeResult[WorkPayload, *schema.Message]{
				RunCtx: ctx,
				// ResumeParams 留空。
			}, nil
		},

		PrepareAgent: func(ctx context.Context, loop *adk.TurnLoop[WorkPayload, *schema.Message], consumed []WorkPayload) (adk.Agent, error) {
			// Pass turnID so the implementation can inject it into tool /
			// agent contexts (for tracing, DB writes, RTC state lookup, etc.).
			tools, err := a.cfg.CreateTools(ctx, sessionID, turnID)
			if err != nil {
				return nil, fmt.Errorf("turnagent: CreateTools: %w", err)
			}
			agent, err := a.cfg.CreateAgent(ctx, sessionID, turnID, tools)
			if err != nil {
				return nil, fmt.Errorf("turnagent: CreateAgent: %w", err)
			}
			return agent, nil
		},

		OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[WorkPayload, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				ev, ok := events.Next()
				if !ok {
					// Agent 已完成事件生产，通知 loop 在当前 turn 结束后退出。
					// Stop() 无参数：让当前 turn 完成，但不开始新 turn。
					// 这解决了 loop 在成功后永久阻塞等待更多 item 的问题。
					tc.Loop.Stop()
					return nil
				}
				if err := a.dispatchEvents(ctx, sessionID, turnID, ev); err != nil {
					tc.Loop.Stop()
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
func (a *Agent) dispatchEvents(ctx context.Context, sessionID, turnID string, ev *adk.AgentEvent) error {
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
		return nil
	}
	mv := ev.Output.MessageOutput

	// 3. Streaming: consume the stream, emit chunks + end.
	if mv.IsStreaming {
		return a.consumeStream(ctx, sessionID, turnID, ev.AgentName, string(mv.Role), mv.ToolName, mv.MessageStream)
	}

	// 4. Non-streaming: emit one message event.
	var tokenUsage *TokenUsage
	if mv.Message != nil && mv.Message.ResponseMeta != nil && mv.Message.ResponseMeta.Usage != nil {
		tokenUsage = extractTokenUsage(mv.Message.ResponseMeta.Usage)
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
func (a *Agent) consumeStream(ctx context.Context, sessionID, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message]) error {
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
