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
//	[system] Prompts (extractAndInjectPrompts — persistent prompts from DB)
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
// injected here on every turn so they remain in the LLM context.
//
// dbMsgs is the in-memory snapshot of DB messages from loadMessages.
// Newly persisted prompt messages are appended to dbMsgs so that
// extractAndInjectPrompts (called after this function) can see them.
//
// For PersistablePrompt commands, TriggerPrompt is skipped from dynamic
// injection (it's handled by extractAndInjectPrompts via DB path).
// SustainPrompt is always dynamically injected.
//
// Returns updated (messages, dbMsgs).
func (h *helpers) injectCommandPrompts(ctx context.Context, sessionID uuid.UUID, messages []*turnagent.Message, dbMsgs []*model.Message) ([]*turnagent.Message, []*model.Message) {
	if h.deps.CommandRegistry == nil {
		return messages, dbMsgs
	}

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
		return messages, dbMsgs
	}

	// Persist TriggerPrompt for commands that implement PersistablePrompt.
	// Newly created messages are appended to dbMsgs so extractAndInjectPrompts
	// can see them on this turn (not just subsequent turns).
	newPromptMsgs := h.persistCommandPromptsIfNeeded(ctx, sessionID, contributions)
	dbMsgs = append(dbMsgs, newPromptMsgs...)

	// Separate system and user contributions.
	// For PersistablePrompt commands, skip TriggerPrompt from dynamic injection
	// (it's handled by extractAndInjectPrompts via the DB path we just updated).
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
// (by Handler layer), extractAndInjectPrompts will have injected them.
// This function detects that case and skips injection to avoid duplicates.
//
// Injection order (final):
//
//	[system] Attachments (TodoList, SessionMemory, UserMemory)
//	[system] Prompts (extractAndInjectPrompts — persistent prompts from DB)
//	[system] Scenarios (this function — fallback when not persisted)
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

	// Check if scenarios have already been persisted as prompt messages.
	// If so, extractAndInjectPrompts has already injected them — skip to avoid duplicates.
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
	// [system] Prompts (injected by extractAndInjectPrompts)
	// [system] Scenarios (injected by this function)
	// [system] Command prompts
	// [conversation history]
	messages = append([]*turnagent.Message{scenarioMsg}, messages...)

	return messages
}

// extractAndInjectPrompts scans dbMsgs for prompt-type messages, converts
// them to system Messages, and prepends them to the message array.
//
// Prompt messages are stored persistently in DB (created by Handler layer)
// and injected here on every turn so they remain in the LLM context.
//
// Role support: prompts with role="user" are injected as user messages
// (will be merged with consecutive user messages by normalizeMessagesForLLM).
//
// Deduplication: when multiple prompt messages share the same dedup key,
// only the latest one is kept. The dedup key is:
// - name:title (for name="command", to distinguish goal/loop prompts)
// - name (for other prompts like "scenarios")
//
// This prevents token waste from duplicate content while preserving the
// most recent version of each prompt.
//
// Note: normalizeMessagesForLLM will later extract all system messages to
// the front of the array, so exact position here is not critical.
func (h *helpers) extractAndInjectPrompts(
	ctx context.Context,
	messages []*turnagent.Message,
	dbMsgs []*model.Message,
) []*turnagent.Message {
	if len(dbMsgs) == 0 {
		return messages
	}

	// Collect prompt messages, deduplicating by dedup key (keep latest).
	// Use ordered map pattern: track insertion order for deterministic output.
	type promptEntry struct {
		dedupKey string
		msg      *turnagent.Message
	}
	seen := make(map[string]int) // dedupKey → index in kept slice
	var kept []promptEntry

	for _, dbMsg := range dbMsgs {
		contentData, err := primitives.ParseContentData(dbMsg.Content)
		if err != nil {
			continue
		}
		if contentData.Type != protocol.ContentTypePrompt {
			continue
		}
		pc, err := primitives.ParsePromptContent(contentData.Data)
		if err != nil {
			h.logger.Debug(ctx, "extractAndInjectPrompts.parse_failed", map[string]any{
				"message_id": dbMsg.ID.String(),
				"error":      err.Error(),
			})
			continue
		}

		// Determine role: use pc.Role if set, otherwise default to system.
		msgRole := turnagent.RoleSystem
		if pc.Role != nil && string(*pc.Role) == "user" {
			msgRole = turnagent.RoleUser
		}

		// Build dedup key: name:title for command prompts, name for others.
		dedupKey := pc.Name
		if pc.Name == "command" && pc.Title != nil && *pc.Title != "" {
			dedupKey = pc.Name + ":" + *pc.Title
		}

		entry := promptEntry{
			dedupKey: dedupKey,
			msg: &turnagent.Message{
				Role:      msgRole,
				Content:   formatPromptAsXML(pc),
				CreatedAt: dbMsg.CreatedAt,
			},
		}
		if idx, exists := seen[dedupKey]; exists {
			// Replace earlier entry with this newer one (same dedup key).
			kept[idx] = entry
		} else {
			seen[dedupKey] = len(kept)
			kept = append(kept, entry)
		}
	}

	if len(kept) == 0 {
		return messages
	}

	// Separate system and user prompts.
	var systemPrompts, userPrompts []*turnagent.Message
	for _, entry := range kept {
		if entry.msg.Role == turnagent.RoleSystem {
			systemPrompts = append(systemPrompts, entry.msg)
		} else {
			userPrompts = append(userPrompts, entry.msg)
		}
	}

	// Prepend system prompts, append user prompts.
	// System prompts go before the conversation history.
	// User prompts go after the conversation history (will be merged with
	// the last user message by normalizeMessagesForLLM).
	if len(systemPrompts) > 0 {
		messages = append(systemPrompts, messages...)
	}
	if len(userPrompts) > 0 {
		messages = append(messages, userPrompts...)
	}
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
// On re-trigger, a new prompt message is always created. extractAndInjectPrompts
// handles dedup (keeps latest per dedup key), so old prompt messages in DB are
// harmless — they remain for prompt cache stability but are not injected.
//
// The persisted prompt message uses role="system" in DB (for frontend rendering)
// but PromptContent.Role controls how it's injected into LLM context.
//
// Returns the newly created model.Message entries so the caller can append them
// to the in-memory dbMsgs snapshot, making them visible to extractAndInjectPrompts.
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
