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
//
// Beyond metrics/logging, the handler also:
//   - Extracts full token dimensions (cached read/write, reasoning)
//   - Calculates cost via model pricing
//   - Atomically accumulates Session-level token usage in SQL
//   - Runs TokenEstimator for compression progress prediction
//   - Throttles Session Update publishing to avoid event storms

package agent

import (
	"context"
	"time"

	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	ucb "github.com/cloudwego/eino/utils/callbacks"
	"github.com/google/uuid"
	dbmodel "github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// newTokenUsageCallbackHandler builds an eino callbacks.Handler that captures
// token usage from both streaming and non-streaming ChatModel calls.
//
// It is a method on helpers so it can access SessionRepo, TokenEstimator,
// Throttle, and model pricing — enabling the full token data flow:
// extract → cost → SQL accumulate → estimate → throttled publish → metrics/log.
func (h *helpers) newTokenUsageCallbackHandler() callbacks.Handler {
	return ucb.NewHandlerHelper().
		ChatModel(&ucb.ModelCallbackHandler{
			// Non-streaming path: OnEnd fires with the complete CallbackOutput.
			OnEnd: func(ctx context.Context, info *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
				modelName := ""
				if output.Config != nil {
					modelName = output.Config.Model
				}
				fullUsage := h.extractFullUsage(output)
				if fullUsage != nil {
					h.reportLLMCall(ctx, fullUsage, modelName)
				}
				return ctx
			},

			// Streaming path: OnEndWithStreamOutput fires with a StreamReader of
			// chunks. Drain it in a goroutine and report once at EOF.
			//
			// Per-call state (maxUsage, modelName, lastMessage, thinkingContent,
			// cachedWriteTokens) is captured in this closure so concurrent streams
			// do not interfere with each other.
			//
			// Use WithoutCancel to decouple from the parent context lifecycle:
			// the goroutine must complete stream draining and token reporting
			// even if the eino framework cancels ctx after callback returns.
			// WithTimeout (60s) prevents the goroutine from hanging indefinitely
			// if the stream stalls.
			OnEndWithStreamOutput: func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[*model.CallbackOutput]) context.Context {
				bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
				go func() {
					defer cancel()
					// No outer recover needed: drainStreamAndReport handles its
					// own panic recovery internally (see its defer/recover block).
					h.drainStreamAndReport(bgCtx, output)
				}()
				return ctx
			},
		}).
		Handler()
}

// reportLLMCall is the shared sink for both streaming and non-streaming paths.
// It performs the full token data flow: extract -> cost -> SQL accumulate -> estimate -> publish -> metrics/log.
func (h *helpers) reportLLMCall(ctx context.Context, fullUsage *FullTokenUsage, modelName string) {
	sessionIDStr := turnagent.SessionIDFromContext(ctx)
	turnIDStr := turnagent.TurnIDFromContext(ctx)

	sessionID, parseErr := uuid.Parse(sessionIDStr)
	if parseErr != nil {
		h.logger.Info(ctx, "token_callback.invalid_session_id", map[string]any{
			"session_id": sessionIDStr,
			"error":      parseErr.Error(),
		})
		return
	}

	costMicros := calculateCostMicros(fullUsage, h.modelPricing)

	session, sessErr := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if sessErr != nil {
		h.logger.Warn(ctx, "token_callback.get_session_failed", map[string]any{
			"session_id": sessionID,
			"error":      sessErr.Error(),
		})
	}

	isCompress := isCompressContext(ctx)
	currentCtxTokens, estimate := h.computeTokenEstimate(ctx, session, sessionID, fullUsage.TotalTokens)

	delta := h.buildTokenUsageDelta(fullUsage, costMicros, isCompress, session, currentCtxTokens, estimate)
	if err := h.deps.SessionRepo.AtomicAddTokenUsage(ctx, sessionID, delta); err != nil {
		h.logger.Warn(ctx, "token_callback.atomic_update_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
	}

	h.applySessionTokenUpdates(session, fullUsage, costMicros, currentCtxTokens, isCompress, estimate)
	h.publishSessionUpdateWithWarnings(ctx, session, sessionID, estimate)

	if h.metrics != nil {
		h.metrics.RecordLLMCall(ctx, turnagent.LLMCallMetricsAttrs{
			SessionID:       sessionIDStr,
			TurnID:          turnIDStr,
			Model:           modelName,
			InputTokens:     int(fullUsage.InputTokens),
			OutputTokens:    int(fullUsage.OutputTokens),
			TotalTokens:     int(fullUsage.TotalTokens),
			CachedTokens:    int(fullUsage.CachedReadTokens),
			ReasoningTokens: int(fullUsage.ReasoningTokens),
		})
	}

	h.logger.Info(ctx, "llm.complete", map[string]any{
		"session_id":          sessionIDStr,
		"turn_id":             turnIDStr,
		"model":               modelName,
		"input_tokens":        fullUsage.InputTokens,
		"output_tokens":       fullUsage.OutputTokens,
		"total_tokens":        fullUsage.TotalTokens,
		"cached_read_tokens":  fullUsage.CachedReadTokens,
		"cached_write_tokens": fullUsage.CachedWriteTokens,
		"reasoning_tokens":    fullUsage.ReasoningTokens,
		"cost_micros":         costMicros,
	})
}

// computeTokenEstimate computes the current context tokens and token estimate
// based on the session state.
func (h *helpers) computeTokenEstimate(ctx context.Context, session *dbmodel.Session, sessionID uuid.UUID, totalTokens int64) (int64, *TokenEstimate) {
	if session == nil {
		return 0, nil
	}
	currentCtxTokens := session.TotalTokens + totalTokens
	if session.CurrentContextTokens > 0 {
		currentCtxTokens = session.CurrentContextTokens + totalTokens
	}
	var estimate *TokenEstimate
	if h.tokenEstimator != nil {
		estimate = h.tokenEstimator.Estimate(ctx, sessionID, currentCtxTokens, session.TokenEstimateEWMA, totalTokens)
	}
	return currentCtxTokens, estimate
}

// buildTokenUsageDelta builds a TokenUsageDelta for the SQL atomic accumulate step.
func (h *helpers) buildTokenUsageDelta(fullUsage *FullTokenUsage, costMicros int64, isCompress bool, session *dbmodel.Session, currentCtxTokens int64, estimate *TokenEstimate) repo.TokenUsageDelta {
	delta := repo.TokenUsageDelta{
		InputDelta:              fullUsage.InputTokens,
		OutputDelta:             fullUsage.OutputTokens,
		TotalDelta:              fullUsage.TotalTokens,
		CachedReadDelta:         fullUsage.CachedReadTokens,
		CachedWriteDelta:        fullUsage.CachedWriteTokens,
		ReasoningDelta:          fullUsage.ReasoningTokens,
		CostMicrosDelta:         costMicros,
		SetCurrentContextTokens: 0,
	}
	if !isCompress && session != nil {
		delta.SetCurrentContextTokens = currentCtxTokens
	}
	if estimate != nil {
		delta.SetEWMA = estimate.NewEWMA
	}
	return delta
}

// applySessionTokenUpdates updates the session struct in-memory with new token counts.
func (h *helpers) applySessionTokenUpdates(session *dbmodel.Session, fullUsage *FullTokenUsage, costMicros, currentCtxTokens int64, isCompress bool, estimate *TokenEstimate) {
	if session == nil || estimate == nil {
		return
	}
	session.TotalTokens += fullUsage.TotalTokens
	if !isCompress {
		session.CurrentContextTokens = currentCtxTokens
	}
	session.TotalInputTokens += fullUsage.InputTokens
	session.TotalOutputTokens += fullUsage.OutputTokens
	session.TotalCachedReadTokens += fullUsage.CachedReadTokens
	session.TotalCachedWriteTokens += fullUsage.CachedWriteTokens
	session.TotalReasoningTokens += fullUsage.ReasoningTokens
	session.TotalCostMicros += costMicros
	session.TokenEstimateEWMA = estimate.NewEWMA
}

// publishSessionUpdateWithWarnings publishes the session update and logs
// warnings about approaching compression or low cache hit rates.
func (h *helpers) publishSessionUpdateWithWarnings(ctx context.Context, session *dbmodel.Session, sessionID uuid.UUID, estimate *TokenEstimate) {
	if session == nil {
		return
	}
	h.publishSessionUpdate(ctx, session, true)

	if estimate != nil && estimate.RoundsUntilCompression >= 0 && estimate.RoundsUntilCompression <= 2 {
		h.logger.Info(ctx, "token_callback.compression_approaching", map[string]any{
			"session_id":            sessionID,
			"current_tokens":        estimate.CurrentTokens,
			"threshold":             estimate.CompressionThreshold,
			"rounds_until_compress": estimate.RoundsUntilCompression,
		})
	}

	if h.cacheHitRateWarnThreshold >= 0 {
		totalCached := session.TotalCachedReadTokens
		totalInput := session.TotalInputTokens
		totalRelevant := totalCached + totalInput
		if totalRelevant > 0 {
			hitRate := float64(totalCached) / float64(totalRelevant)
			if hitRate < h.cacheHitRateWarnThreshold {
				logger.Warn(ctx, "cache hit rate below threshold",
					zap.String("session_id", sessionID.String()),
					zap.Float64("cache_hit_rate", hitRate),
					zap.Float64("threshold", h.cacheHitRateWarnThreshold),
					zap.Int64("cached_read_tokens", totalCached),
					zap.Int64("input_tokens", totalInput),
				)
			}
		}
	}
}

// estimateClaudeReasoningTokens estimates reasoning tokens for Claude.
// This is a fallback for when CompletionTokensDetails.ReasoningTokens is not set.
// If the message has thinking content, returns OutputTokens as the estimate.
func estimateClaudeReasoningTokens(msg *schema.Message, outputTokens int) int {
	if msg == nil {
		return 0
	}
	thinking, ok := einoclaude.GetThinking(msg)
	if !ok || thinking == "" {
		return 0
	}
	return outputTokens
}

// extractFullUsage extracts complete token usage dimensions from a
// CallbackOutput, including cache read/write and reasoning tokens.
//
// Reasoning tokens extraction uses a fallback strategy:
// 1. Try CompletionTokensDetails.ReasoningTokens (OpenAI, Qwen, etc.)
// 2. Fallback: if Claude has thinking content, use OutputTokens as estimate
func (h *helpers) extractFullUsage(output *model.CallbackOutput) *FullTokenUsage {
	if output == nil || output.TokenUsage == nil {
		return nil
	}
	usage := output.TokenUsage

	var cachedWriteTokens int
	var reasoningTokens int
	if output.Message != nil {
		if v, ok := einoclaude.GetCacheCreationInputTokens(output.Message); ok {
			cachedWriteTokens = v
		}
		reasoningTokens = estimateClaudeReasoningTokens(output.Message, usage.CompletionTokens)
	}

	if usage.CompletionTokensDetails.ReasoningTokens > 0 {
		reasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}

	cachedReadTokens := usage.PromptTokenDetails.CachedTokens
	pureInput := usage.PromptTokens - cachedReadTokens - cachedWriteTokens
	if pureInput < 0 {
		pureInput = 0
	}

	return &FullTokenUsage{
		InputTokens:       int64(pureInput),
		OutputTokens:      int64(usage.CompletionTokens),
		CachedReadTokens:  int64(cachedReadTokens),
		CachedWriteTokens: int64(cachedWriteTokens),
		ReasoningTokens:   int64(reasoningTokens),
		TotalTokens:       int64(usage.TotalTokens),
	}
}

// mergeTokenUsageMax merges src into dst, keeping the maximum of each field.
func mergeTokenUsageMax(dst *model.TokenUsage, src *model.TokenUsage) {
	if src == nil {
		return
	}
	if src.PromptTokens > dst.PromptTokens {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens > dst.CompletionTokens {
		dst.CompletionTokens = src.CompletionTokens
	}
	if src.TotalTokens > dst.TotalTokens {
		dst.TotalTokens = src.TotalTokens
	}
	if src.PromptTokenDetails.CachedTokens > dst.PromptTokenDetails.CachedTokens {
		dst.PromptTokenDetails.CachedTokens = src.PromptTokenDetails.CachedTokens
	}
	if src.CompletionTokensDetails.ReasoningTokens > dst.CompletionTokensDetails.ReasoningTokens {
		dst.CompletionTokensDetails.ReasoningTokens = src.CompletionTokensDetails.ReasoningTokens
	}
}

// drainStreamAndReport drains a streaming CallbackOutput reader, aggregates
// token usage across chunks, and reports the final usage once.
//
// It runs in its own goroutine (spawned by OnEndWithStreamOutput) and handles
// panic recovery internally. Per-call state is local to the goroutine so
// concurrent streams do not interfere with each other.
//
// Per-read timeout: unlike consumeStream (which uses RecvWithTimeout for the
// main LLM stream), this function calls output.Recv() without a per-read
// timeout. This is acceptable because:
//  1. The caller (OnEndWithStreamOutput) wraps ctx with a 60s overall timeout,
//     bounding the goroutine's lifetime.
//  2. The StreamReader.Recv() is channel-based and returns promptly when the
//     LLM call completes (stream Close triggers io.EOF on the channel).
//  3. The stream type (*model.CallbackOutput) differs from the main LLM stream
//     (*schema.Message), so the existing RecvWithTimeout cannot be reused
//     without a generic version — the complexity is not justified given (1).
func (h *helpers) drainStreamAndReport(ctx context.Context, output *schema.StreamReader[*model.CallbackOutput]) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(context.Background(), "token_callback.stream_drain_panic",
				zap.Any("recover", r),
			)
		}
	}()
	defer output.Close()

	var maxUsage model.TokenUsage
	var modelName string
	var lastMessage *schema.Message
	var thinkingContent string
	var cachedWriteTokens int

	for {
		chunk, err := output.Recv()
		if err != nil {
			break // EOF or stream error -- report whatever we accumulated.
		}
		if chunk.Config != nil && chunk.Config.Model != "" {
			modelName = chunk.Config.Model
		}
		mergeTokenUsageMax(&maxUsage, chunk.TokenUsage)
		if chunk.Message != nil {
			lastMessage = chunk.Message
			if thinking, ok := einoclaude.GetThinking(chunk.Message); ok && thinking != "" {
				thinkingContent += thinking
			}
			if v, ok := einoclaude.GetCacheCreationInputTokens(chunk.Message); ok && v > cachedWriteTokens {
				cachedWriteTokens = v
			}
		}
	}

	fullOutput := h.buildDrainedOutput(thinkingContent, lastMessage, modelName, &maxUsage)
	fullUsage := h.extractFullUsage(fullOutput)
	if fullUsage != nil {
		applyCachedWriteOverride(fullUsage, cachedWriteTokens)
		h.reportLLMCall(ctx, fullUsage, modelName)
	}
}

// buildDrainedOutput constructs a synthetic CallbackOutput from accumulated
// stream chunks, merging thinking content into the last message.
func (h *helpers) buildDrainedOutput(thinkingContent string, lastMessage *schema.Message, modelName string, maxUsage *model.TokenUsage) *model.CallbackOutput {
	if thinkingContent != "" && lastMessage != nil {
		msgCopy := *lastMessage
		if msgCopy.Extra == nil {
			msgCopy.Extra = make(map[string]any)
		}
		msgCopy.Extra["_eino_claude_thinking"] = thinkingContent
		lastMessage = &msgCopy
	}
	return &model.CallbackOutput{
		Message:    lastMessage,
		Config:     &model.Config{Model: modelName},
		TokenUsage: maxUsage,
	}
}

// applyCachedWriteOverride overrides the cached write tokens with the
// accumulated max when it's larger, and recalculates input tokens accordingly.
func applyCachedWriteOverride(fullUsage *FullTokenUsage, cachedWriteTokens int) {
	if int64(cachedWriteTokens) <= fullUsage.CachedWriteTokens {
		return
	}
	fullUsage.CachedWriteTokens = int64(cachedWriteTokens)
	promptTokens := fullUsage.TotalTokens - fullUsage.OutputTokens
	pureInput := promptTokens - fullUsage.CachedReadTokens - fullUsage.CachedWriteTokens
	if pureInput < 0 {
		pureInput = 0
	}
	fullUsage.InputTokens = pureInput
}
