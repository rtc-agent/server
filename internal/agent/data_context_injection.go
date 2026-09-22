package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/channel"
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
//	[user/assistant] Conversation history (includes prompt messages at natural position)

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
// injected here on every turn so they remain in the LLM context.
//
// For PersistablePrompt commands, TriggerPrompt is persisted to DB and
// included in the conversation history by convertDBMessage at its natural
// position (based on global_offset). SustainPrompt is always dynamically
// injected (not persisted).
//
// Returns updated (messages, dbMsgs).
func (h *helpers) injectCommandPrompts(ctx context.Context, sessionID uuid.UUID, messages []*turnagent.Message, dbMsgs []*model.Message) ([]*turnagent.Message, []*model.Message) {
	if h.deps.CommandRegistry == nil {
		return messages, dbMsgs
	}

	// Extract last user message content, but ONLY if the last message overall
	// is a user message. This prevents re-detecting commands from historical
	// user messages when the conversation has moved on (e.g., after assistant
	// responses or tool results).
	//
	// Command detection should only trigger on fresh user input, not on
	// historical messages. If the last message is assistant/tool, the user
	// hasn't sent a new command, so we skip detection entirely.
	var lastUserContent string
	if len(messages) > 0 && messages[len(messages)-1].Role == turnagent.RoleUser {
		lastUserContent = messages[len(messages)-1].Content
	}

	cmdCtx := command.Context{
		Context:   ctx,
		SessionID: sessionID,
	}
	contributions, err := h.deps.CommandRegistry.DetectAndInject(cmdCtx, lastUserContent)
	if err != nil || len(contributions) == 0 {
		return messages, dbMsgs
	}

	// Persist TriggerPrompt for commands that implement PersistablePrompt.
	// Newly created messages are appended to dbMsgs so they can be converted
	// by convertDBMessage on this turn (not just subsequent turns).
	newPromptMsgs := h.persistCommandPromptsIfNeeded(ctx, sessionID, contributions)
	dbMsgs = append(dbMsgs, newPromptMsgs...)

	// Separate system and user contributions.
	// For PersistablePrompt commands, skip TriggerPrompt from dynamic injection
	// (it's persisted to DB and included by convertDBMessage).
	// SustainPrompt is always dynamically injected (not persisted).
	var systemMsgs, userMsgs []*turnagent.Message
	for _, nc := range contributions {
		// Skip TriggerPrompt for PersistablePrompt commands only.
		// SustainPrompt (IsNewlyTriggered=false) is always injected dynamically.
		if cmd := h.deps.CommandRegistry.FindByName(nc.CommandName); cmd != nil {
			if _, ok := cmd.(command.PersistablePrompt); ok {
				if h.deps.CommandRegistry.IsNewlyTriggered(sessionID, nc.CommandName) {
					continue
				}
			}
		}
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
	return messages, dbMsgs
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
// Detection: if scenarios have already been persisted as prompt messages
// (by Handler layer), convertDBMessage will include them.
// This function detects that case and skips injection to avoid duplicates.
//
// Injection order (final):
//
//	[system] Attachments (TodoList, SessionMemory, UserMemory)
//	[system] Scenarios (this function — fallback when not persisted)
//	[system] Command prompts (/goal, /persona, etc.)
//	[user/assistant] Conversation history (includes persisted prompt messages)
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

	// Check if scenarios have already been persisted as prompt messages.
	// If so, convertDBMessage will include them — skip to avoid duplicates.
	for _, dbMsg := range dbMsgs {
		contentData, err := primitives.ParseContentData(dbMsg.Content)
		if err != nil {
			continue
		}
		if contentData.Type == protocol.ContentTypePrompt {
			pc, err := primitives.ParsePromptContent(contentData.Data)
			if err != nil {
				continue
			}
			// Found a persisted scenario prompt — skip injection.
			if pc.Name == "scenarios" {
				return messages
			}
		}
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
	// [conversation history] (includes persisted prompt messages)
	messages = append([]*turnagent.Message{scenarioMsg}, messages...)

	return messages
}

// formatPromptAsXML wraps prompt content in XML tags for LLM consumption.
// Format: <prompt name="..." title="...">content</prompt>
func formatPromptAsXML(pc protocol.PromptContent) string {
	var sb strings.Builder
	sb.WriteString("<prompt")
	fmt.Fprintf(&sb, ` name="%s"`, escapeXMLAttr(pc.Name))
	if pc.Title != nil && *pc.Title != "" {
		fmt.Fprintf(&sb, ` title="%s"`, escapeXMLAttr(*pc.Title))
	}
	sb.WriteString(">\n")
	sb.WriteString(escapeXMLContent(pc.Prompt))
	sb.WriteString("\n</prompt>")
	return sb.String()
}

// persistCommandPromptsIfNeeded persists TriggerPrompt contributions as prompt
// messages in DB. Only persists when:
// 1. The command was newly triggered this turn (not sustain)
// 2. The command implements PersistablePrompt interface
//
// Dedup is not needed here because command detection only fires on fresh user
// input (last message is user role), and genResume skips detection entirely.
//
// The persisted prompt message uses role="system" in DB (for frontend rendering)
// but PromptContent.Role controls how it's injected into LLM context.
//
// Returns the newly created model.Message entries so the caller can append them
// to the in-memory dbMsgs snapshot.
func (h *helpers) persistCommandPromptsIfNeeded(
	ctx context.Context,
	sessionID uuid.UUID,
	contributions []command.NamedContribution,
) []*model.Message {
	var newMessages []*model.Message
	for _, nc := range contributions {
		// Only persist newly triggered commands (not sustain).
		if !h.deps.CommandRegistry.IsNewlyTriggered(sessionID, nc.CommandName) {
			continue
		}

		// Check if the command supports persistence.
		cmd := h.deps.CommandRegistry.FindByName(nc.CommandName)
		if cmd == nil {
			continue
		}
		persistable, ok := cmd.(command.PersistablePrompt)
		if !ok {
			continue
		}
		config := persistable.PromptPersistConfig()
		if !config.Persist {
			continue
		}

		// Determine role for the prompt message.
		// This role is stored in PromptContent and controls how the prompt is
		// injected into LLM context (as user or system message).
		promptRole := nc.Contribution.Role
		if promptRole == "" {
			promptRole = "system"
		}

		// Build PromptContent with role field.
		// The role field in PromptContent controls how the prompt is injected
		// into LLM context (as user or system message), but the DB message
		// itself always uses role="system" for consistent frontend rendering.
		contentData, err := primitives.PromptContentDataWithRole(
			config.Name, config.Title, nc.Contribution.Content, promptRole,
		)
		if err != nil {
			h.logger.Warn(ctx, "persistCommandPrompts.build_content_failed", map[string]any{
				"command": nc.CommandName,
				"error":   err.Error(),
			})
			continue
		}

		// Get session to retrieve topic channel.
		session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
		if err != nil || session == nil {
			h.logger.Warn(ctx, "persistCommandPrompts.get_session_failed", map[string]any{
				"command": nc.CommandName,
				"error":   err,
			})
			continue
		}
		topicCh := channel.UserTopic(session.OwnerRefID)

		// DB message always uses role="system" for consistent frontend rendering.
		// The PromptContent.Role field controls LLM context injection role.
		newMsg, err := h.createAndPublishMessage(
			ctx,
			sessionID,
			uuid.Nil,
			protocol.MessageRoleSystem,
			contentData,
			protocol.MessageStreamingCompleted,
			topicCh,
		)
		if err != nil {
			h.logger.Warn(ctx, "persistCommandPrompts.failed", map[string]any{
				"command": nc.CommandName,
				"error":   err.Error(),
			})
		} else {
			h.logger.Debug(ctx, "persistCommandPrompts.persisted", map[string]any{
				"command": nc.CommandName,
				"name":    config.Name,
				"title":   config.Title,
				"role":    promptRole,
			})
			newMessages = append(newMessages, newMsg)
		}
	}
	return newMessages
}
