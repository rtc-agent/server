package agent

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
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
	memories, err := h.deps.SessionMemoryRepo.ListBySession(ctx, sessionID, 20)
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

	// Build summary from memories
	summary := buildSummaryFromMemories(memories)

	h.logger.Info(ctx, "compressContextWithSessionMemory.success", map[string]any{
		"session_id":   sessionID.String(),
		"memory_count": len(memories),
	})

	return &summary
}

// buildSummaryFromMemories builds session memories into summary text.
// Groups by category and formats as readable markdown. Template is defined in
// prompts/attachments/session-memory-summary.md.tmpl。
func buildSummaryFromMemories(memories []*model.SessionMemory) string {
	return buildSummaryFromMemoriesTmpl(memories)
}

// formatSessionMemoriesForInjection formats session memories for injection into messages.
// Used for pre-turn injection (different from compression summary). Template defined in
// prompts/attachments/session-memory-injection.md.tmpl.
func formatSessionMemoriesForInjection(memories []*model.SessionMemory) string {
	return formatSessionMemoryInjection(memories)
}
