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
	// reportLLMCall is the shared sink for both streaming and non-streaming paths.
	reportLLMCall := func(ctx context.Context, fullUsage *FullTokenUsage, modelName string) {
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		turnIDStr := turnagent.TurnIDFromContext(ctx)

		sessionID, _ := uuid.Parse(sessionIDStr)

		// ===============================================================
		// Step 1: Calculate cost
		// ===============================================================
		costMicros := calculateCostMicros(fullUsage, h.modelPricing)

		// ===============================================================
		// Step 2: Read session to get prevEWMA + current TotalTokens
		// ===============================================================
		session, sessErr := h.deps.SessionRepo.GetByID(ctx, sessionID)
		if sessErr != nil {
			h.logIfEnabled(ctx, "token_callback.get_session_failed", map[string]any{
				"session_id": sessionID,
				"error":      sessErr.Error(),
			})
		}

		// ===============================================================
		// Step 3: Compute token estimate (EWMA + derived fields)
		// ===============================================================
		var estimate *TokenEstimate
		if session != nil && h.tokenEstimator != nil {
			prevEWMA := session.TokenEstimateEWMA
			estimate = h.tokenEstimator.Estimate(ctx, sessionID, session.TotalTokens+fullUsage.TotalTokens, prevEWMA, fullUsage.TotalTokens)
		}

		// ===============================================================
		// Step 4: SQL atomic accumulate Session token usage + persist EWMA
		// ===============================================================
		delta := repo.TokenUsageDelta{
			InputDelta:       fullUsage.InputTokens,
			OutputDelta:      fullUsage.OutputTokens,
			TotalDelta:       fullUsage.TotalTokens,
			CachedReadDelta:  fullUsage.CachedReadTokens,
			CachedWriteDelta: fullUsage.CachedWriteTokens,
			ReasoningDelta:   fullUsage.ReasoningTokens,
			CostMicrosDelta:  costMicros,
		}
		if estimate != nil {
			delta.SetEWMA = estimate.NewEWMA
		}

		if err := h.deps.SessionRepo.AtomicAddTokenUsage(ctx, sessionID, delta); err != nil {
			h.logIfEnabled(ctx, "token_callback.atomic_update_failed", map[string]any{
				"session_id": sessionID,
				"error":      err.Error(),
			})
		}

		// Update session with new TotalTokens for publishing
		if session != nil && estimate != nil {
			session.TotalTokens += fullUsage.TotalTokens
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
				h.logIfEnabled(ctx, "token_callback.compression_approaching", map[string]any{
					"session_id":            sessionID,
					"current_tokens":        estimate.CurrentTokens,
					"threshold":             estimate.CompressionThreshold,
					"rounds_until_compress": estimate.RoundsUntilCompression,
				})
			}

			// Cache hit rate warning: alert when session cumulative hit rate is below threshold.
			// Negative threshold disables the alert.
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
		// Step 5: Metrics + log (extended dimensions)
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

		if h.logger != nil {
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
	}

	// estimateClaudeReasoningTokens estimates reasoning tokens for Claude.
	// This is a fallback for when CompletionTokensDetails.ReasoningTokens is not set.
	// If the message has thinking content, returns OutputTokens as the estimate
	// (since thinking message's output tokens ≈ reasoning tokens in our design).
	// Returns 0 if no thinking content is found.
	estimateClaudeReasoningTokens := func(msg *schema.Message, outputTokens int) int {
		if msg == nil {
			return 0
		}
		thinking, ok := einoclaude.GetThinking(msg)
		if !ok || thinking == "" {
			return 0
		}
		// In our design, thinking messages carry reasoning content.
		// The OutputTokens for a call with thinking ≈ reasoning tokens.
		// This is an approximation but more accurate than character-based estimation.
		return outputTokens
	}

	// extractFullUsage extracts complete token usage dimensions from a
	// CallbackOutput, including cache read/write and reasoning tokens.
	//
	// Reasoning tokens extraction uses a fallback strategy:
	// 1. Try CompletionTokensDetails.ReasoningTokens (OpenAI, Qwen, etc.)
	// 2. Fallback: if Claude has thinking content, use OutputTokens as estimate
	extractFullUsage := func(output *model.CallbackOutput) *FullTokenUsage {
		if output == nil || output.TokenUsage == nil {
			return nil
		}
		usage := output.TokenUsage

		// Cache write tokens from Claude-specific message extra
		var cachedWriteTokens int
		var reasoningTokens int
		if output.Message != nil {
			if v, ok := einoclaude.GetCacheCreationInputTokens(output.Message); ok {
				cachedWriteTokens = v
			}
			// Fallback for reasoning tokens: if Claude has thinking content,
			// use OutputTokens as the estimate (thinking message's output ≈ reasoning tokens).
			// This is needed because eino's Claude provider doesn't map
			// Anthropic's OutputTokensDetails.ThinkingTokens to CompletionTokensDetails.ReasoningTokens
			reasoningTokens = estimateClaudeReasoningTokens(output.Message, usage.CompletionTokens)
		}

		// Primary source: CompletionTokensDetails.ReasoningTokens (OpenAI, Qwen, etc.)
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

	return ucb.NewHandlerHelper().
		ChatModel(&ucb.ModelCallbackHandler{
			// Non-streaming path: OnEnd fires with the complete CallbackOutput.
			OnEnd: func(ctx context.Context, info *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
				modelName := ""
				if output.Config != nil {
					modelName = output.Config.Model
				}
				fullUsage := extractFullUsage(output)
				if fullUsage != nil {
					reportLLMCall(ctx, fullUsage, modelName)
				}
				return ctx
			},

			// Streaming path: OnEndWithStreamOutput fires with a StreamReader of
			// chunks. Drain it in a goroutine and report once at EOF.
			//
			// Per-call state (maxUsage, modelName, lastMessage, thinkingContent) is captured in
			// this closure so concurrent streams do not interfere with each other.
			OnEndWithStreamOutput: func(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[*model.CallbackOutput]) context.Context {
				// Per-call accumulator — safe for concurrent invocations because
				// each OnEndWithStreamOutput call creates its own closure frame.
				var maxUsage model.TokenUsage
				var modelName string
				var lastMessage *schema.Message
				var thinkingContent string // Accumulate thinking content from all chunks

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
					// Merge cache read tokens (max across chunks)
					if u.PromptTokenDetails.CachedTokens > maxUsage.PromptTokenDetails.CachedTokens {
						maxUsage.PromptTokenDetails.CachedTokens = u.PromptTokenDetails.CachedTokens
					}
					// Merge reasoning tokens (max across chunks)
					if u.CompletionTokensDetails.ReasoningTokens > maxUsage.CompletionTokensDetails.ReasoningTokens {
						maxUsage.CompletionTokensDetails.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
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
						// Track last message for cache write token extraction
						if chunk.Message != nil {
							lastMessage = chunk.Message
							// Accumulate thinking content from Claude streaming chunks
							if thinking, ok := einoclaude.GetThinking(chunk.Message); ok && thinking != "" {
								thinkingContent += thinking
							}
						}
					}

					// Build a synthetic CallbackOutput for extractFullUsage
					// If we accumulated thinking content, attach it to the last message
					if thinkingContent != "" && lastMessage != nil {
						// Create a copy of lastMessage with accumulated thinking content
						msgCopy := *lastMessage
						// Set thinking content in message extra (same key as eino's Claude provider)
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
					fullUsage := extractFullUsage(fullOutput)
					if fullUsage != nil {
						reportLLMCall(ctx, fullUsage, modelName)
					}
				}()

				return ctx
			},
		}).
		Handler()
}
