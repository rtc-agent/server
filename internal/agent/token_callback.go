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
					h.drainStreamAndReport(bgCtx, output)
				}()
				return ctx
			},
		}).
		Handler()
}

// reportLLMCall is the shared sink for both streaming and non-streaming paths.
// It performs the full token data flow: extract → cost → SQL accumulate → estimate → publish → metrics/log.
func (h *helpers) reportLLMCall(ctx context.Context, fullUsage *FullTokenUsage, modelName string) {
	sessionIDStr := turnagent.SessionIDFromContext(ctx)
	turnIDStr := turnagent.TurnIDFromContext(ctx)

	sessionID, parseErr := uuid.Parse(sessionIDStr)
	if parseErr != nil {
		h.logger.Info(ctx, "token_callback.invalid_session_id", map[string]any{
			"session_id": sessionIDStr,
			"error":      parseErr.Error(),
		})
		return // 无法继续处理无效的 session ID
	}

	// ===============================================================
	// Step 1: Calculate cost
	// ===============================================================
	costMicros := calculateCostMicros(fullUsage, h.modelPricing)

	// ===============================================================
	// Step 2: Read session to get prevEWMA + current TotalTokens
	// ===============================================================
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if sessErr != nil {
		h.logger.Info(ctx, "token_callback.get_session_failed", map[string]any{
			"session_id": sessionID,
			"error":      sessErr.Error(),
		})
	}

	// BUG-08 fix: Check if this is a compression LLM call.
	// Compression calls should NOT update CurrentContextTokens because
	// persistCompressedMessages already writes the accurate post-compression value.
	// Without this check, the callback would overwrite with tokensAfter + compressionTokens,
	// causing CurrentContextTokens to be overestimated.
	isCompress := isCompressContext(ctx)

	// ===============================================================
	// Step 3: Compute token estimate (EWMA + derived fields)
	// ===============================================================
	var currentCtxTokens int64
	var estimate *TokenEstimate
	if session != nil {
		currentCtxTokens = session.TotalTokens + fullUsage.TotalTokens
		if session.CurrentContextTokens > 0 {
			currentCtxTokens = session.CurrentContextTokens + fullUsage.TotalTokens
		}
		if h.tokenEstimator != nil {
			prevEWMA := session.TokenEstimateEWMA
			estimate = h.tokenEstimator.Estimate(ctx, sessionID, currentCtxTokens, prevEWMA, fullUsage.TotalTokens)
		}
	}

	// ===============================================================
	// Step 4: SQL atomic accumulate Session token usage + persist EWMA
	// ===============================================================
	delta := repo.TokenUsageDelta{
		InputDelta:              fullUsage.InputTokens,
		OutputDelta:             fullUsage.OutputTokens,
		TotalDelta:              fullUsage.TotalTokens,
		CachedReadDelta:         fullUsage.CachedReadTokens,
		CachedWriteDelta:        fullUsage.CachedWriteTokens,
		ReasoningDelta:          fullUsage.ReasoningTokens,
		CostMicrosDelta:         costMicros,
		SetCurrentContextTokens: 0, // default: don't update
	}

	// BUG-08 fix: Only update CurrentContextTokens for non-compression calls.
	if !isCompress && session != nil {
		delta.SetCurrentContextTokens = currentCtxTokens
	}

	if estimate != nil {
		delta.SetEWMA = estimate.NewEWMA
	}

	if err := h.deps.SessionRepo.AtomicAddTokenUsage(ctx, sessionID, delta); err != nil {
		h.logger.Info(ctx, "token_callback.atomic_update_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
	}

	// Update session with new TotalTokens for publishing
	if session != nil && estimate != nil {
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

	// ===============================================================
	// Step 5: Throttled Session Update publish
	// ===============================================================
	if session != nil {
		h.publishSessionUpdate(ctx, session, true)

		// Compression warning: alert when approaching threshold
		if estimate != nil && estimate.RoundsUntilCompression >= 0 && estimate.RoundsUntilCompression <= 2 {
			h.logger.Info(ctx, "token_callback.compression_approaching", map[string]any{
				"session_id":            sessionID,
				"current_tokens":        estimate.CurrentTokens,
				"threshold":             estimate.CompressionThreshold,
				"rounds_until_compress": estimate.RoundsUntilCompression,
			})
		}

		// Cache hit rate warning: alert when session cumulative hit rate is below threshold.
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

	// ===============================================================
	// Step 6: Metrics + log (extended dimensions)
	// ===============================================================
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

// drainStreamAndReport drains a streaming CallbackOutput reader, aggregates
// token usage across chunks, and reports the final usage once.
//
// It runs in its own goroutine (spawned by OnEndWithStreamOutput) and handles
// panic recovery internally. Per-call state is local to the goroutine so
// concurrent streams do not interfere with each other.
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
		if u.PromptTokenDetails.CachedTokens > maxUsage.PromptTokenDetails.CachedTokens {
			maxUsage.PromptTokenDetails.CachedTokens = u.PromptTokenDetails.CachedTokens
		}
		if u.CompletionTokensDetails.ReasoningTokens > maxUsage.CompletionTokensDetails.ReasoningTokens {
			maxUsage.CompletionTokensDetails.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
		}
	}

	for {
		chunk, err := output.Recv()
		if err != nil {
			break // EOF or stream error — report whatever we accumulated.
		}
		if chunk.Config != nil && chunk.Config.Model != "" {
			modelName = chunk.Config.Model
		}
		mergeUsage(chunk.TokenUsage)
		if chunk.Message != nil {
			lastMessage = chunk.Message
			if thinking, ok := einoclaude.GetThinking(chunk.Message); ok && thinking != "" {
				thinkingContent += thinking
			}
			// BUG-02 fix: accumulate cache creation input tokens across chunks.
			if v, ok := einoclaude.GetCacheCreationInputTokens(chunk.Message); ok && v > cachedWriteTokens {
				cachedWriteTokens = v
			}
		}
	}

	// Build a synthetic CallbackOutput for extractFullUsage.
	if thinkingContent != "" && lastMessage != nil {
		msgCopy := *lastMessage
		if msgCopy.Extra == nil {
			msgCopy.Extra = make(map[string]any)
		}
		msgCopy.Extra["_eino_claude_thinking"] = thinkingContent
		lastMessage = &msgCopy
	}
	fullOutput := &model.CallbackOutput{
		Message:    lastMessage,
		Config:     &model.Config{Model: modelName},
		TokenUsage: &maxUsage,
	}
	fullUsage := h.extractFullUsage(fullOutput)
	if fullUsage != nil {
		// BUG-02 fix: streaming path lost cache creation tokens because
		// message_start (which carries them) has empty Content and gets
		// overwritten by later chunks; message_delta reports 0. Override
		// with the accumulated max when it's larger.
		if int64(cachedWriteTokens) > fullUsage.CachedWriteTokens {
			fullUsage.CachedWriteTokens = int64(cachedWriteTokens)
			promptTokens := fullUsage.TotalTokens - fullUsage.OutputTokens
			pureInput := promptTokens - fullUsage.CachedReadTokens - fullUsage.CachedWriteTokens
			if pureInput < 0 {
				pureInput = 0
			}
			fullUsage.InputTokens = pureInput
		}
		h.reportLLMCall(ctx, fullUsage, modelName)
	}
}
