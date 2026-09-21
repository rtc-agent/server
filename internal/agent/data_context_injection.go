package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Context Injection — Command & Scenario Prompt Injection
// =============================================================================
//
// These functions inject dynamic system-level prompts into the message array
// before it reaches the LLM. They run AFTER loadMessages loads the conversation
// history but BEFORE attachments are prepended.
//
// Final message order after all injection steps:
//
//	[system] Attachments (TodoList, SessionMemory, UserMemory)
//	[system] Scenarios (injectScenarioPrompts)
//	[system] Command prompts (injectCommandPrompts)
//	[user/assistant] Conversation history

// injectCommandPrompts runs the slash-command framework's DetectAndInject,
// converting the returned PromptContributions to turnagent Messages.
// If the registry has no commands or none match, this is a no-op.
//
// Each contributed prompt is wrapped with an XML tag identifying the
// contributing command, so the LLM can distinguish sources:
//
//	<command name="persona">...</command>
//
// Message ordering: System messages are prepended to the message array
// (Claude API requires system messages at the start). User messages are
// appended to the end. This ensures valid message sequence for the LLM.
func (h *helpers) injectCommandPrompts(ctx context.Context, sessionID uuid.UUID, messages []*turnagent.Message) []*turnagent.Message {
	if h.deps.CommandRegistry == nil {
		return messages
	}

	// Pre-activate commands from DB state before DetectAndInject.
	// DetectAndInject only detects commands from the last user message prefix,
	// but resume turns may not have a command-triggering message. Without
	// DB-based pre-activation, SustainPrompt is skipped on servers that
	// didn't handle the original trigger turn.
	//
	// Note: createTools already calls ensureCommandsActivated earlier in the
	// turn, so this is a defense-in-depth measure. Since EnsureActivated is
	// idempotent, the second call is a no-op when commands are already active.
	h.ensureCommandsActivated(ctx, sessionID)

	// Extract last user message content.
	var lastUserContent string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == turnagent.RoleUser {
			lastUserContent = messages[i].Content
			break
		}
	}

	cmdCtx := command.Context{
		Context:   ctx,
		SessionID: sessionID,
	}
	contributions, err := h.deps.CommandRegistry.DetectAndInject(cmdCtx, lastUserContent)
	if err != nil || len(contributions) == 0 {
		return messages
	}

	// Separate system and user contributions.
	// System messages must be at the start of the message array per Claude API.
	var systemMsgs, userMsgs []*turnagent.Message
	for _, nc := range contributions {
		msg := &turnagent.Message{
			Role:    nc.Contribution.Role,
			Content: wrapWithTag(nc.CommandName, nc.Contribution.Content),
		}
		if nc.Contribution.Role == turnagent.RoleSystem {
			systemMsgs = append(systemMsgs, msg)
		} else {
			userMsgs = append(userMsgs, msg)
		}
	}

	// Prepend system messages, append user messages.
	if len(systemMsgs) > 0 {
		messages = append(systemMsgs, messages...)
	}
	if len(userMsgs) > 0 {
		messages = append(messages, userMsgs...)
	}
	return messages
}

func wrapWithTag(name, content string) string {
	return "<command name=\"" + escapeXMLAttr(name) + "\">\n" + escapeXMLContent(content) + "\n</command>"
}

// injectScenarioPrompts injects scenario content as system prompts.
// Extracts scenarios from the last user message's ContentData, wraps each
// scenario's FileContent in <scenario> tags, and injects as a system message
// at the beginning of the message array.
//
// dbMsgs is the already-loaded DB messages from loadMessages, passed to avoid
// a redundant DB query. The scenarios field is only available in the raw
// model.Message (lost during convertDBMessage).
//
// Injection order (final):
//
//	[system] Attachments (TodoList, SessionMemory, UserMemory)
//	[system] Scenarios (this function)
//	[system] Command prompts (/goal, /persona, etc.)
//	[user/assistant] Conversation history
func (h *helpers) injectScenarioPrompts(
	messages []*turnagent.Message,
	dbMsgs []*model.Message,
) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	// Reuse the caller's already-loaded dbMsgs to avoid redundant DB queries.
	// Scenario information is only available in the raw model.Message (lost
	// after convertDBMessage).
	if len(dbMsgs) == 0 {
		return messages
	}

	// Find the last user message.
	var lastUserMsg *model.Message
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		if dbMsgs[i].Role == string(schema.User) {
			lastUserMsg = dbMsgs[i]
			break
		}
	}

	if lastUserMsg == nil {
		return messages
	}

	// Parse ContentData.
	contentData, err := primitives.ParseContentData(lastUserMsg.Content)
	if err != nil {
		return messages
	}

	// Only process user_message type.
	if contentData.Type != protocol.ContentTypeUserMessage {
		return messages
	}

	// Parse UserMessageContent.
	umc, err := primitives.ParseUserMessageContent(contentData.Data)
	if err != nil {
		return messages
	}

	// Check if scenarios exist.
	if umc.Scenarios == nil || len(*umc.Scenarios) == 0 {
		return messages
	}

	// Build scenario prompts.
	var scenarioPrompts []string
	for _, scenario := range *umc.Scenarios {
		if scenario.Title != "" && scenario.FileContent != "" {
			scenarioPrompts = append(scenarioPrompts,
				fmt.Sprintf("<scenario title=\"%s\">\n%s\n</scenario>",
					escapeXMLAttr(scenario.Title),
					escapeXMLContent(scenario.FileContent)))
		}
	}

	if len(scenarioPrompts) == 0 {
		return messages
	}

	// Concatenate all scenarios.
	combinedScenarios := strings.Join(scenarioPrompts, "\n\n")

	// Create system message.
	scenarioMsg := &turnagent.Message{
		Role:    turnagent.RoleSystem,
		Content: fmt.Sprintf("<scenarios>\n%s\n</scenarios>", combinedScenarios),
	}

	// Insert at the beginning of the message array (system messages must come first).
	// Note: attachments will be prepended later, so the final order is:
	// [system] Attachments
	// [system] Scenarios (injected by this function)
	// [system] Command prompts
	// [conversation history]
	messages = append([]*turnagent.Message{scenarioMsg}, messages...)

	return messages
}
