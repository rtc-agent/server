// internal/agent/compact.go
//
// processCompactWorker is the CompactContext callback for explicit /compact
// commands. It loads messages, compresses the context (via compressContext),
// and logs compression stats.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/google/uuid"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// processCompactWorker is the CompactContext callback wired into turnagent.Config.
//
// Flow:
//  1. Inject sessionID into context (needed by compressContext).
//  2. Initialize eino callbacks so compact's LLM calls are tracked.
//  3. Load messages via the same callback used by eino's GenInput.
//  4. Count tokens before compression.
//  5. Call compressContext (handles LLM summarization + persistence).
//  6. Count tokens after compression.
//  7. Re-estimate token usage and publish fresh estimate to frontend.
//  8. Log compression stats.
func (h *helpers) processCompactWorker(ctx context.Context, sessionID string, customInstruction *string) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("compact: invalid session ID %q: %w", sessionID, err)
	}

	h.logger.Info(ctx, "compact.start", map[string]any{
		"session_id":             sessionID,
		"has_custom_instruction": customInstruction != nil,
	})

	// 1. Inject sessionID into context so compressContext can find it.
	// Use WithoutCancel to decouple from the parent context lifecycle:
	// compact is a background operation that should complete even if the
	// triggering context (e.g., client connection) is canceled. The publish
	// operations inside must persist state to the database/frontend regardless
	// of the caller's lifecycle.
	compactCtx := withSessionID(context.WithoutCancel(ctx), sid)
	// Also set the turnagent sessionID key for downstream helpers.
	compactCtx = turnagent.WithSessionID(compactCtx, sessionID)

	// Initialize eino callbacks in the compact context.
	// The compact flow bypasses the turn loop (session_manager.go GenInput),
	// so callbacks.InitCallbacks is never called. Without this, the token
	// usage callback handler would not fire for compact's LLM calls, and
	// compact's token consumption would not be recorded to Session.TotalTokens.
	// Note: turnID is intentionally not set — compact is not a turn.
	if h.tokenCallbackHandler != nil {
		compactCtx = callbacks.InitCallbacks(compactCtx, &callbacks.RunInfo{}, h.tokenCallbackHandler)
	}

	// 2. Load messages.
	agentMsgs, err := h.loadMessages(compactCtx, sessionID)
	if err != nil {
		return fmt.Errorf("compact: load messages: %w", err)
	}

	schemaMsgs := turnagent.MessagesToEino(agentMsgs)
	if len(schemaMsgs) == 0 {
		h.logger.Info(ctx, "compact.no_messages", map[string]any{
			"session_id": sessionID,
		})
		return nil
	}

	// 3. Count tokens before compression.
	tokensBefore, _ := cumulativeTokenCounter(ctx, schemaMsgs)

	// 3b. Save the pre-compact EWMA before compressContext runs.
	// The token callback inside compressContext will update the EWMA with an
	// EWMA based on the compact's LLM cost (not normal turn growth), so we need
	// to save the real pre-compact EWMA here.
	var prevEWMA float64
	if h.tokenEstimator != nil {
		prevEWMA = h.tokenEstimator.ReadEWMA(compactCtx, sid)
	}

	// 4. Compress context (handles LLM summarization + streaming + persistence).
	// force=true because this is a manual /compact command.
	start := time.Now()
	compressed, err := h.compressContext(compactCtx, schemaMsgs, customInstruction, true)
	if err != nil {
		return fmt.Errorf("compact: compress context: %w", err)
	}

	// 5. Count tokens after compression.
	tokensAfter, _ := cumulativeTokenCounter(ctx, compressed)

	// 5b. After compression, write back current_context_tokens so the frontend progress bar reflects the real context size.
	if tokensAfter > 0 {
		if err := h.deps.SessionRepo.Update(compactCtx, sid, map[string]any{
			"current_context_tokens": tokensAfter,
		}); err != nil {
			h.logger.Warn(ctx, "compact.update_context_tokens_failed", map[string]any{
				"session_id": sessionID,
				"error":      err.Error(),
			})
		}
	}

	// 6. Re-estimate token usage after compression.
	// This replaces the stale pre-compact estimate with a fresh one based on
	// the new TotalTokens baseline. The EWMA is adjusted by the compression
	// ratio so the growth rate tracks the compressed context size.
	if h.tokenEstimator != nil {
		estimate, estimateErr := h.tokenEstimator.ReestimateAfterCompact(compactCtx, sid, prevEWMA, tokensBefore, tokensAfter)
		if estimateErr != nil {
			h.logger.Warn(ctx, "compact.reestimate_failed", map[string]any{
				"session_id": sessionID,
				"error":      estimateErr.Error(),
			})
		} else if estimate != nil {
			h.logger.Info(ctx, "compact.reestimate", map[string]any{
				"session_id":               sessionID,
				"current_tokens":           estimate.CurrentTokens,
				"estimated_next_round":     estimate.EstimatedNextRound,
				"rounds_until_compression": estimate.RoundsUntilCompression,
			})

			// Publish a session update so the frontend sees the fresh estimate.
			if session, sessErr := h.deps.SessionRepo.GetByID(compactCtx, sid); sessErr == nil && session != nil {
				h.publishSessionUpdate(compactCtx, session, true)
			}
		}
	}

	// 7. Log compression stats.
	duration := time.Since(start)
	ratio := 0.0
	if tokensBefore > 0 {
		ratio = float64(tokensAfter) / float64(tokensBefore)
	}

	h.logger.Info(ctx, "compact.completed", map[string]any{
		"session_id":    sessionID,
		"tokens_before": tokensBefore,
		"tokens_after":  tokensAfter,
		"ratio":         ratio,
		"duration_ms":   duration.Milliseconds(),
	})

	return nil
}
