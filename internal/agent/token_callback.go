// token_callback.go provides an eino callbacks.Handler that records LLM token
// usage to metrics and structured logging on every ChatModel call.
//
// The handler is registered on Config.Callbacks in New(), so it is injected
// into every turn's context via callbacks.InitCallbacks in GenInput. This
// automatically covers all ChatModel invocations — the main agent loop and
// the summarizeMessages compression call — without any extra wiring.

package agent

import (
	"context"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	ucb "github.com/cloudwego/eino/utils/callbacks"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// newTokenUsageCallbackHandler builds an eino callbacks.Handler that fires
// on every ChatModel OnEnd. It extracts token usage and model name from
// model.CallbackOutput, then reports metrics and a structured log line.
//
// sessionID and turnID are read from context (injected by GenInput via
// WithSessionID / WithTurnID in pkg/turn-agent/types.go).
func newTokenUsageCallbackHandler(metrics turnagent.Metrics, logger turnagent.Logger) callbacks.Handler {
	return ucb.NewHandlerHelper().
		ChatModel(&ucb.ModelCallbackHandler{
			OnEnd: func(ctx context.Context, info *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
				var inputTokens, outputTokens, totalTokens int
				if output.TokenUsage != nil {
					inputTokens = output.TokenUsage.PromptTokens
					outputTokens = output.TokenUsage.CompletionTokens
					totalTokens = output.TokenUsage.TotalTokens
				}

				modelName := ""
				if output.Config != nil {
					modelName = output.Config.Model
				}

				sessionID := turnagent.SessionIDFromContext(ctx)
				turnID := turnagent.TurnIDFromContext(ctx)

				// finish_reason 可能为空（output.Message 或 ResponseMeta 为 nil）。
				finishReason := ""
				if output.Message != nil && output.Message.ResponseMeta != nil {
					finishReason = output.Message.ResponseMeta.FinishReason
				}

				// Metrics 上报
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

				// 结构化日志
				if logger != nil {
					logger.Info(ctx, "llm.complete", map[string]any{
						"session_id":    sessionID,
						"turn_id":       turnID,
						"model":         modelName,
						"input_tokens":  inputTokens,
						"output_tokens": outputTokens,
						"total_tokens":  totalTokens,
						"finish_reason": finishReason,
					})
				}

				return ctx
			},
		}).
		Handler()
}
