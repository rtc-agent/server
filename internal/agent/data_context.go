package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Data callbacks
// =============================================================================
//
// These four callbacks handle the data flow between the agent and the
// application. They use turn-agent's own types (Message, Event) — the
// application never writes code that consumes eino's flow primitives.

// loadMessages loads the conversation history for a session from the DB and
// converts it to turn-agent Message types.
//
// Called by eino's GenInput path (fresh turns only — eino does not call
// GenInput when resuming from a checkpoint).
//
// Mapping from old code: this replaces loadMessageHistory in
// internal/worker/context.go. The key difference is the return type:
// []*turnagent.Message instead of []*schema.Message. The conversion from
// model.Message to turnagent.Message mirrors the old conversion to
// schema.Message, but uses the pkg-level types.
func (h *helpers) loadMessages(ctx context.Context, sessionID string) ([]*turnagent.Message, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: invalid session ID %q: %w", sessionID, err)
	}

	// Load recent messages from DB
	const historyLimit = 200
	dbMsgs, err := h.deps.MessageRepo.ListRecentBySession(ctx, sid, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: list messages for session %s: %w", sessionID, err)
	}

	// Summary truncation: find the most recent summary message and truncate
	// history to start from it. Messages before the summary are already
	// compressed into it and would not be sent to the agent.
	tmpMsgs := make([]*model.Message, 0, len(dbMsgs))
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		msg := dbMsgs[i]
		tmpMsgs = append(tmpMsgs, msg)
		contentData, parseErr := primitives.ParseContentData(msg.Content)
		if parseErr == nil && contentData.Type == protocol.ContentTypeSummary {
			break
		}
	}
	sort.Slice(tmpMsgs, func(i, j int) bool {
		return tmpMsgs[i].GlobalOffset < tmpMsgs[j].GlobalOffset
	})
	dbMsgs = tmpMsgs

	// Convert DB messages to turn-agent Messages.
	// The conversion logic mirrors the old SchemaMessages method in context.go,
	// but produces turnagent.Message instead of schema.Message.
	//
	// convertDBMessage may return nil for unparseable or unrecognized content
	// types; skip those to prevent nil entries from reaching the LLM adapter
	// (which would produce nil schema.Message entries and risk a panic).
	var messages []*turnagent.Message
	for _, msg := range dbMsgs {
		converted := convertDBMessage(msg)
		for _, cm := range converted {
			if cm != nil {
				messages = append(messages, cm)
			}
		}
	}

	// Merge consecutive thinking + text messages from the same assistant turn.
	// The streaming handler stores thinking and text as separate DB messages;
	// when loaded back, the thinking-only message has Content="" which causes
	// the Claude adapter to produce an empty text block fallback.
	// This merge eliminates those empty-content messages before they reach the LLM.
	messages = mergeAssistantMessages(messages)

	// Apply context management: tool result budget and microcompact
	messages = applyToolResultBudget(messages, h.toolResultBudgetConfig())
	messages = microcompactMessages(messages, h.microcompactConfig())

	// Build and inject all attachments (TodoList, SessionMemory, UserMemory).
	// Attachments are dynamic content that provides the LLM with persistent
	// context beyond the conversation history.
	if len(messages) > 0 && h.attachmentManager != nil {
		// Get userID from session
		var userID uuid.UUID
		if session, sessionErr := h.deps.SessionRepo.GetByID(ctx, sid); sessionErr == nil && session != nil {
			if parsedID, parseErr := uuid.Parse(session.OwnerRefID); parseErr == nil {
				userID = parsedID
			}
		}

		// Build attachments
		attachmentMsgs, err := h.attachmentManager.BuildAttachments(ctx, sid, userID)
		if err != nil {
			h.logIfEnabled(ctx, "loadMessages.build_attachments_failed", map[string]any{
				"session_id": sid.String(),
				"error":      err.Error(),
			})
		} else if len(attachmentMsgs) > 0 {
			messages = append(messages, attachmentMsgs...)
		}
	}

	// Trigger background Session Memory extraction (async, non-blocking).
	// The extractor checks whether extraction is needed based on token growth
	// and tool call count.
	if len(messages) > 0 {
		h.triggerSessionMemoryExtraction(ctx, sid, messages)
	}

	// Slash-command framework. Detect any command prefix on the last user
	// message, update per-session activation state, and append the
	// commands' prompt contributions. The /goal command is now handled by
	// GoalWorkflow registered in the registry (see goal_workflow.go).
	messages = h.injectCommandPrompts(ctx, sid, messages)

	return messages, nil
}

// convertDBMessage converts a single model.Message to zero or more
// turnagent.Messages.
//
// A single DB message may produce multiple turnagent messages (e.g., a
// toolcall_input produces an assistant message with tool calls; a summary
// may expand into multiple messages).
func convertDBMessage(msg *model.Message) []*turnagent.Message {
	contentData, err := primitives.ParseContentData(msg.Content)
	if err != nil {
		return nil
	}

	// Build TokenUsage from DB fields (populated for assistant messages).
	var tokenUsage *turnagent.TokenUsage
	if msg.TotalTokens != nil {
		tokenUsage = &turnagent.TokenUsage{
			TotalTokens:  *msg.TotalTokens,
			InputTokens:  intDeref(msg.InputTokens),
			OutputTokens: intDeref(msg.OutputTokens),
		}
		if msg.CachedTokens != nil {
			tokenUsage.CachedTokens = *msg.CachedTokens
		}
		if msg.ReasoningTokens != nil {
			tokenUsage.ReasoningTokens = *msg.ReasoningTokens
		}
	}

	switch contentData.Type {
	case protocol.ContentTypeSummary:
		return convertSummaryContent(contentData.Data, msg.CreatedAt)

	case protocol.ContentTypeText, protocol.ContentTypeMarkdown:
		text, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:       msg.Role,
			Content:    text,
			TokenUsage: tokenUsage,
			CreatedAt:  msg.CreatedAt,
		}}

	case protocol.ContentTypeThinking:
		tk, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:             msg.Role,
			ReasoningContent: tk,
			CreatedAt:        msg.CreatedAt,
		}}

	case protocol.ContentTypeToolCallInput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil
		}
		// Produce an assistant message with tool calls.
		return []*turnagent.Message{{
			Role: string(schema.Assistant),
			ToolCalls: []turnagent.ToolCall{{
				ID:        string(toolCall.Id),
				Name:      toolCall.ToolName,
				Arguments: toolCall.Input,
			}},
			CreatedAt: msg.CreatedAt,
		}}

	case protocol.ContentTypeToolCallOutput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil
		}
		content := formatToolCallOutput(toolCall)
		return []*turnagent.Message{{
			Role:       string(schema.Tool),
			Content:    content,
			ToolName:   toolCall.ToolName,
			ToolCallID: string(toolCall.Id),
			CreatedAt:  msg.CreatedAt,
		}}

	default:
		return nil
	}
}

// intDeref safely dereferences a *int, returning 0 for nil.
func intDeref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// convertSummaryContent expands a summary content block into multiple messages.
// Supports both old format ([]SummaryItem) and new format (SummaryContent{items, metadata}).
func convertSummaryContent(data any, createdAt time.Time) []*turnagent.Message {
	dataBytes, err := primitives.ContentDataBytes(data)
	if err != nil {
		return nil
	}

	// Try new format first (SummaryContent with items and metadata)
	var content primitives.SummaryContent
	if err := json.Unmarshal(dataBytes, &content); err == nil && content.Items != nil {
		return buildMessagesFromSummaryItems(content.Items, createdAt)
	}

	// Fallback to old format ([]SummaryItem)
	var items []primitives.SummaryItem
	if err := json.Unmarshal(dataBytes, &items); err == nil {
		return buildMessagesFromSummaryItems(items, createdAt)
	}

	return nil
}

// buildMessagesFromSummaryItems converts SummaryItem slice to turnagent messages.
func buildMessagesFromSummaryItems(items []primitives.SummaryItem, createdAt time.Time) []*turnagent.Message {
	var msgs []*turnagent.Message
	for _, item := range items {
		msgs = append(msgs, &turnagent.Message{
			Role:      item.Role,
			Content:   item.Content,
			CreatedAt: createdAt,
		})
	}
	return msgs
}

// mergeAssistantMessages merges consecutive thinking + text/tool messages from
// the same assistant turn into single messages.
//
// The streaming handler (handleStreamChunk) stores thinking and markdown as
// separate DB messages. When loaded back via convertDBMessage, a thinking-only
// message has Content="" and ReasoningContent set. The eino-ext Claude adapter
// reads thinking from Extra["_eino_claude_thinking"] (not ReasoningContent),
// so without Extra populated the message appears empty. The adapter's fallback
// then produces {"text": "", "type": "text"} — the empty content blocks seen
// in the LLM-API log.
//
// This function fixes that by:
//  1. Merging thinking -> text pairs: the thinking's ReasoningContent is moved
//     into the next assistant text message's ReasoningContent, and the
//     thinking-only message is dropped.
//  2. Dropping standalone thinking messages followed by a non-assistant message
//     (typically a tool result). Converting these to text would create an
//     orphan assistant{text} before the tool{result}, breaking the tool-call
//     pairing contract required by the Claude API. The reasoning is implicitly
//     preserved in the tool call's arguments.
//  3. Converting trailing thinking messages (last in the conversation, no
//     following message at all) to text so the content is preserved.
//
// This is an in-memory-only transformation; the DB is not modified.
func mergeAssistantMessages(messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	result := make([]*turnagent.Message, 0, len(messages))
	i := 0
	for i < len(messages) {
		msg := messages[i]

		// Skip nil entries (can occur when convertDBMessage returns nil).
		if msg == nil {
			i++
			continue
		}

		// Check for thinking-only assistant message: has ReasoningContent
		// but no Content, no ToolCalls.
		if msg.Role == turnagent.RoleAssistant &&
			msg.ReasoningContent != "" &&
			msg.Content == "" &&
			len(msg.ToolCalls) == 0 {

			// Look ahead for the next assistant text/tool message.
			next := i + 1
			for next < len(messages) &&
				messages[next].Role == turnagent.RoleAssistant &&
				messages[next].Content == "" &&
				messages[next].ReasoningContent != "" &&
				len(messages[next].ToolCalls) == 0 {
				// Skip consecutive thinking-only messages — accumulate
				// their reasoning into the current one.
				msg.ReasoningContent += "\n" + messages[next].ReasoningContent
				next++
			}

			if next < len(messages) && messages[next].Role == turnagent.RoleAssistant {
				// Merge: move thinking into the next message's ReasoningContent.
				messages[next].ReasoningContent = msg.ReasoningContent
				// Drop the thinking-only message; advance past it.
				i = next
				continue
			}

			if next < len(messages) {
				// Followed by a non-assistant message (typically tool result).
				// Drop the thinking to avoid:
				//   1. Empty content block (adapter fallback: {text: ""})
				//   2. Orphan assistant{text} before tool{result} which
				//      breaks tool call pairing in the Claude API.
				// The reasoning is implicitly preserved in the tool call's
				// arguments (e.g., the file path the model decided to read).
				i++
				continue
			}

			// Trailing thinking (last message in conversation): convert to
			// text so the content is preserved as a text block.
			msg.Content = msg.ReasoningContent
			msg.ReasoningContent = ""
		}

		result = append(result, msg)
		i++
	}
	return result
}

// injectCommandPrompts runs the slash-command framework's DetectAndInject,
// converting the returned PromptContributions to turnagent Messages.
// If the registry has no commands or none match, this is a no-op.
//
// Each contributed prompt is wrapped with an XML tag identifying the
// contributing command, so the LLM can distinguish sources:
//
//	<command name="persona">…</command>
//
// Message ordering: System messages are prepended to the message array
// (Claude API requires system messages at the start). User messages are
// appended to the end. This ensures valid message sequence for the LLM.
func (h *helpers) injectCommandPrompts(goCtx context.Context, sessionID uuid.UUID, messages []*turnagent.Message) []*turnagent.Message {
	if h.deps.CommandRegistry == nil {
		return messages
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
		Context:   goCtx,
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
	return "<command name=\"" + name + "\">\n" + content + "\n</command>"
}
