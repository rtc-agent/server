// internal/agent/compact.go
//
// processCompactWorker is the CompactContext callback for explicit /compact
// commands. It loads messages, compresses the context (via compressContext),
// and pushes compression stats to the Live channel.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// processCompactWorker is the CompactContext callback wired into turnagent.Config.
//
// Flow:
//  1. Inject sessionID into context (needed by compressContext).
//  2. Load messages via the same callback used by eino's GenInput.
//  3. Count tokens before compression.
//  4. Call compressContext (handles LLM summarization + persistence).
//  5. Count tokens after compression.
//  6. Push compression stats to the Live channel.
func (h *helpers) processCompactWorker(ctx context.Context, sessionID string, customInstruction *string) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("compact: invalid session ID %q: %w", sessionID, err)
	}

	h.logIfEnabled(ctx, "compact.start", map[string]any{
		"session_id":           sessionID,
		"has_custom_instruction": customInstruction != nil,
	})

	// 1. Inject sessionID into context so compressContext can find it.
	compactCtx := withSessionID(ctx, sid)
	// Also set the turnagent sessionID key for downstream helpers.
	compactCtx = turnagent.WithSessionID(compactCtx, sessionID)

	// 2. Load messages.
	agentMsgs, err := h.loadMessages(compactCtx, sessionID)
	if err != nil {
		return fmt.Errorf("compact: load messages: %w", err)
	}

	schemaMsgs := turnagent.MessagesToEino(agentMsgs)
	if len(schemaMsgs) == 0 {
		h.logIfEnabled(ctx, "compact.no_messages", map[string]any{
			"session_id": sessionID,
		})
		return nil
	}

	// 3. Count tokens before compression.
	tokensBefore, _ := cumulativeTokenCounter(ctx, schemaMsgs)

	// 4. Compress context (handles LLM summarization + streaming + persistence).
	// force=true because this is a manual /compact command.
	start := time.Now()
	compressed, err := h.compressContext(compactCtx, schemaMsgs, customInstruction, true)
	if err != nil {
		return fmt.Errorf("compact: compress context: %w", err)
	}

	// 5. Count tokens after compression.
	tokensAfter, _ := cumulativeTokenCounter(ctx, compressed)

	// 6. Push stats to Live channel.
	duration := time.Since(start)
	ratio := 0.0
	if tokensBefore > 0 {
		ratio = float64(tokensAfter) / float64(tokensBefore)
	}

	h.logIfEnabled(ctx, "compact.completed", map[string]any{
		"session_id":    sessionID,
		"tokens_before": tokensBefore,
		"tokens_after":  tokensAfter,
		"ratio":         ratio,
		"duration_ms":   duration.Milliseconds(),
	})

	return nil
}
