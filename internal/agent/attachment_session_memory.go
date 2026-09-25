package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/pkg/memory"
)

// SessionMemoryAttachment injects session memories into LLM context.
//
// Session memories are automatically extracted from the conversation by the
// SessionMemoryExtractor. They capture key decisions, progress, issues, and
// learnings from the current session.
//
// This attachment queries the most recent memories (up to 5, with a total
// token budget of 5000) and injects them so the LLM can maintain context
// about what has happened in the session.
type SessionMemoryAttachment struct {
	helpers *helpers
}

// NewSessionMemoryAttachment creates a new SessionMemoryAttachment.
func NewSessionMemoryAttachment(h *helpers) *SessionMemoryAttachment {
	return &SessionMemoryAttachment{helpers: h}
}

// Name returns the attachment name.
func (a *SessionMemoryAttachment) Name() string {
	return "SessionMemory"
}

// Build generates the session memory content for injection.
//
// Returns empty string if:
// - No session memories exist
// - Query fails
//
// The output is formatted using memory.NewFormatter().FormatForInjection,
// which groups memories by type and formats them as a system reminder.
func (a *SessionMemoryAttachment) Build(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (string, error) {
	// Query recent session memories for injection
	memories, err := a.helpers.deps.MemoryRepo.ListRecentForInjection(ctx, memory.ScopeSession, sessionID, 5, 5000)
	if err != nil {
		return "", fmt.Errorf("query session memories: %w", err)
	}

	if len(memories) == 0 {
		return "", nil
	}

	// Format for injection using the unified memory formatter
	return memory.NewFormatter().FormatForInjection(memories, "en"), nil
}
