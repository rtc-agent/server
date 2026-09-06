package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// AgentPromptAttachment injects the AGENT.md content snapshot into LLM context.
//
// The agent_prompt is stored in the Session model when the session is created
// (via action.sendmessage). This attachment ensures the LLM always has the
// agent's identity, responsibilities, and behavioral rules available, without
// requiring a wasteful first-turn read tool call.
//
// It is always injected (not just on the first turn) so that after context
// compression the LLM still has the complete AGENT.md context.
type AgentPromptAttachment struct {
	helpers *helpers
}

// NewAgentPromptAttachment creates a new AgentPromptAttachment.
func NewAgentPromptAttachment(h *helpers) *AgentPromptAttachment {
	return &AgentPromptAttachment{helpers: h}
}

// Name returns the attachment name.
func (a *AgentPromptAttachment) Name() string {
	return "AgentPrompt"
}

// Build generates the agent prompt content for injection.
//
// Returns empty string if:
// - The session has no agent_prompt (e.g. sessions created before this feature)
//
// The output is wrapped in <system-reminder> tags so the LLM treats it as
// system-level context rather than user input.
func (a *AgentPromptAttachment) Build(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (string, error) {
	session, err := a.helpers.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	if session.AgentPrompt == "" {
		return "", nil
	}

	return "<system-reminder>\n" + session.AgentPrompt + "\n</system-reminder>", nil
}
