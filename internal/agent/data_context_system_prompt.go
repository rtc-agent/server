package agent

import (
	"context"

	"github.com/google/uuid"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// injectSystemAndAgentPrompt injects the agent prompt and system prompt as
// system-role messages at the beginning of the message array.
//
// The agent prompt (identity + workflow + capabilities) is injected first,
// followed by the system prompt (universal behavioral rules).
//
// If the session doesn't have an agent prompt, the default agent prompt is used.
//
// This replaces the previous approach of passing these via eino's Instruction
// field, which was inconsistently handled between GenInput and GenResume paths:
//   - GenInput: eino adds Instruction as system message → present in API request
//   - GenResume: HistoryModifier replaces all messages → Instruction lost
//
// By injecting as system messages in the loadMessages pipeline, the content is
// consistently present in both paths, ensuring stable cache prefixes.
//
// Final message order after full loadMessages pipeline:
//
//	[system] AgentPrompt (identity + workflow + capabilities, default or custom)
//	[system] SystemPrompt (behavioral rules)
//	[system] Attachments (SessionMemory, UserMemory)
//	[system] Command prompts
//	[system] Scenarios
//	[user/assistant/tool] Conversation history
func (h *helpers) injectSystemAndAgentPrompt(
	ctx context.Context,
	sid uuid.UUID,
	messages []*turnagent.Message,
) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	var systemMsgs []*turnagent.Message

	// 1. Agent prompt (always present — default or custom)
	// Agent prompt provides identity, workflow, and capabilities.
	agentPrompt := ""
	session, err := h.deps.SessionRepo.GetByID(ctx, sid)
	if err != nil {
		h.logger.Warn(ctx, "injectSystemAndAgentPrompt.get_session_failed", map[string]any{
			"session_id": sid.String(),
			"error":      err.Error(),
		})
	} else if session != nil {
		agentPrompt = session.AgentPrompt
	}

	// Fallback to default agent prompt if session doesn't have one
	if agentPrompt == "" {
		agentPrompt = GetDefaultAgentPrompt()
	}

	if agentPrompt != "" {
		systemMsgs = append(systemMsgs, &turnagent.Message{
			Role:    turnagent.RoleSystem,
			Content: agentPrompt,
		})
	}

	// 2. System prompt (always present — behavioral rules)
	// System prompt provides universal behavioral guidelines.
	systemPrompt := h.deps.SystemPrompt
	if systemPrompt == "" {
		built, err := BuildDefaultSystemPrompt()
		if err != nil {
			h.logger.Warn(ctx, "injectSystemAndAgentPrompt.build_system_prompt_failed", map[string]any{
				"session_id": sid.String(),
				"error":      err.Error(),
			})
		} else {
			systemPrompt = built
		}
	}
	if systemPrompt != "" {
		systemMsgs = append(systemMsgs, &turnagent.Message{
			Role:    turnagent.RoleSystem,
			Content: systemPrompt,
		})
	}

	if len(systemMsgs) == 0 {
		return messages
	}

	// Prepend system messages to the front of the array.
	// normalizeMessagesForLLM's extractSystemMessages will ensure all system
	// messages are at the leading position, so the exact insertion point
	// doesn't matter as long as they're added before normalization.
	return append(systemMsgs, messages...)
}
