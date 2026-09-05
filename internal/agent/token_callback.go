// token_callback.go provides an eino callbacks.Handler that records LLM token
// usage to metrics and structured logging on every ChatModel call.
//
// The handler is registered on Config.Callbacks in New(), so it is injected
// into every turn's context via callbacks.InitCallbacks in GenInput. This
// automatically covers all ChatModel invocations — the main agent loop and
// the summarizeMessages compression call — without any extra wiring.
//
// Streaming vs non-streaming:
//   - Non-streaming (Generate): OnEnd fires with the final CallbackOutput.
//   - Streaming (Stream): OnEndWithStreamOutput fires with a StreamReader of
//     CallbackOutput chunks. We drain the stream in a goroutine, aggregate
//     TokenUsage by taking the max of each field across all chunks, then
//     report once per ChatModel call.

package agent

import (
	"context"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	ucb "github.com/cloudwego/eino/utils/callbacks"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// newTokenUsageCallbackHandler builds an eino callbacks.Handler that captures
// token usage from both streaming and non-streaming ChatModel calls.
//
// For streaming (the common path), OnEndWithStreamOutput receives a
// StreamReader[*model.CallbackOutput] — one per chunk. We drain it in a
// goroutine, take the max of each TokenUsage field across all chunks, and
// call RecordLLMCall once when the stream ends. This mirrors the max-aggregation
// strategy used in turn-agent/agent.go consumeStream.
//
// For non-streaming (summarize, tool-call Generate, etc.), OnEnd receives the
// final CallbackOutput directly.
//
// sessionID and turnID are read from context (injected by GenInput via
// WithSessionID / WithTurnID in pkg/turn-agent/types.go).
func newTokenUsageCallbackHandler(metrics turnagent.Metrics, logger turnagent.Logger) callbacks.Handler {
	// reportLLMCall is the shared sink for both streaming and non-streaming paths.
	reportLLMCall := func(ctx context.Context, modelName string, usage *model.TokenUsage) {
		var inputTokens, outputTokens, totalTokens int
		if usage != nil {
			inputTokens = usage.PromptTokens
			outputTokens = usage.CompletionTokens
			totalTokens = usage.TotalTokens
		}

		sessionID := turnagent.SessionIDFromContext(ctx)
		turnID := turnagent.TurnIDFromContext(ctx)

		if metrics != nil {
			metrics.RecordLLMCall(ctx, turnagent.LLMCallMetricsAttrs{
				SessionID:    sessionID,
				TurnID:       turnID,
				Model:        modelName,
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
				TotalTokens:  totalTokens,
			})
		}

		if logger != nil {
			logger.Info(ctx, "llm.complete", map[string]any{
				"session_id":    sessionID,
				"turn_id":       turnID,
				"model":         modelName,
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
				"total_tokens":  totalTokens,
			})
		}
	}

	return ucb.NewHandlerHelper().
		ChatModel(&ucb.ModelCallbackHandler{
			// Non-streaming path: OnEnd fires with the complete CallbackOutput.
			OnEnd: func(ctx context.Context, info *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
				modelName := ""
				if output.Config != nil {
					modelName = output.Config.Model
				}
				reportLLMCall(ctx, modelName, output.TokenUsage)
				return ctx
			},

			// Streaming path: OnEndWithStreamOutput fires with a StreamReader of
			// chunks. Drain it in a goroutine and report once at EOF.
			//
			// Per-call state (maxUsage, modelName) is captured in this closure so
			// concurrent streams do not interfere with each other.
			OnEndWithStreamOutput: func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[*model.CallbackOutput]) context.Context {
				// Per-call accumulator — safe for concurrent invocations because
				// each OnEndWithStreamOutput call creates its own closure frame.
				var maxUsage model.TokenUsage
				var modelName string

				mergeUsage := func(u *model.TokenUsage) {
					if u == nil {
						return
					}
					if u.PromptTokens > maxUsage.PromptTokens {
						maxUsage.PromptTokens = u.PromptTokens
					}
					if u.CompletionTokens > maxUsage.CompletionTokens {
						maxUsage.CompletionTokens = u.CompletionTokens
					}
					if u.TotalTokens > maxUsage.TotalTokens {
						maxUsage.TotalTokens = u.TotalTokens
					}
				}

				go func() {
					defer output.Close()
					for {
						chunk, err := output.Recv()
						if err != nil {
							break // EOF or stream error — report whatever we accumulated.
						}
						if chunk.Config != nil && chunk.Config.Model != "" {
							modelName = chunk.Config.Model
						}
						mergeUsage(chunk.TokenUsage)
					}
					reportLLMCall(ctx, modelName, &maxUsage)
				}()

				return ctx
			},
		}).
		Handler()
}
