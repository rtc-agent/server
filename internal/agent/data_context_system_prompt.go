package agent

import (
	"context"

	"github.com/google/uuid"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// injectSystemAndAgentPrompt injects the system prompt and agent prompt as
// system-role messages at the beginning of the message array.
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
//	[system] SystemPrompt (identity + workflow + rules)
//	[system] AgentPrompt (AGENT.md content, if session has one)
//	[system] Attachments (TodoList, SessionMemory, UserMemory)
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

	// 1. System prompt (identity + workflow + rules)
	systemPrompt := h.deps.SystemPrompt
	if systemPrompt == "" {
		built, err := BuildDefaultSystemPrompt()
		if err != nil {
			h.logger.Warn(ctx, "injectSystemAndAgentPrompt.build_failed", map[string]any{
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

	// 2. Agent prompt (AGENT.md content from session)
	session, err := h.deps.SessionRepo.GetByID(ctx, sid)
	if err != nil {
		h.logger.Warn(ctx, "injectSystemAndAgentPrompt.get_session_failed", map[string]any{
			"session_id": sid.String(),
			"error":      err.Error(),
		})
	} else if session != nil && session.AgentPrompt != "" {
		systemMsgs = append(systemMsgs, &turnagent.Message{
			Role:    turnagent.RoleSystem,
			Content: session.AgentPrompt,
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
