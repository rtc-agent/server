package agent

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/pkg/memory"
)

// compressContextWithSessionMemory attempts compression using session memories.
// If there are no session memories, returns nil to let the caller fall back to LLM-generated summary.
//
// Strategy:
//  1. Query session memories for the current session
//  2. If memories exist, build summary from them (zero API cost)
//  3. Otherwise, return nil to fall back to LLM summarization
//
// Errors during memory query are logged and treated as "no memories" (fall back
// to LLM), so no error is returned to the caller.
func (h *helpers) compressContextWithSessionMemory(
	ctx context.Context,
) *string {
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return nil // No session ID, fall back to LLM
	}

	// Query session memories
	memories, err := h.deps.MemoryRepo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Limit: 20})
	if err != nil {
		h.logger.Warn(ctx, "compressContextWithSessionMemory.query_error", map[string]any{
			"session_id": sessionID.String(),
			"error":      err.Error(),
		})
		return nil // Fall back to LLM on error
	}

	if len(memories) == 0 {
		return nil // No memories, fall back to LLM
	}

	// Build summary from memories using the unified memory formatter
	summary := memory.NewFormatter().FormatForSummary(memories)

	h.logger.Info(ctx, "compressContextWithSessionMemory.success", map[string]any{
		"session_id":   sessionID.String(),
		"memory_count": len(memories),
	})

	return &summary
}
