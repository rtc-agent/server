package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
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

	// Filter out meaningless thinking messages (e.g., "...\\n") from interrupt/resume.
	messages = filterMeaninglessThinking(messages)

	// Merge consecutive thinking + text messages from the same assistant turn
	// to prevent empty-content messages from reaching the LLM adapter.
	messages = mergeAssistantMessages(messages)

	// Apply context management: tool result budget and microcompact
	messages = applyToolResultBudget(messages, h.toolResultBudgetConfig())
	messages = microcompactMessages(messages, h.microcompactConfig())

	// Slash-command framework. Detect any command prefix on the last user
	// message, update per-session activation state, and append the
	// commands' prompt contributions. The /goal command is now handled by
	// GoalWorkflow registered in the registry (see goal_workflow.go).
	// NOTE: This is done BEFORE attachments so that command system prompts
	// (like /goal) appear after attachments in the final message order:
	// [system] Attachments → [system] Command prompts → [conversation]
	messages = h.injectCommandPrompts(ctx, sid, messages)

	// Inject scenario prompts from the last user message's scenarios field.
	// Scenarios are injected as a system message after command prompts but
	// before attachments are prepended, so the final order is:
	// [system] Attachments → [system] Scenarios → [system] Command prompts → [conversation]
	// Pass the already-loaded dbMsgs to avoid a redundant DB query.
	messages = h.injectScenarioPrompts(messages, dbMsgs)

	// Build and inject all attachments (TodoList, SessionMemory, UserMemory).
	// Attachments are dynamic content that provides the LLM with persistent
	// context beyond the conversation history.
	//
	// Attachments are prepended to the message array (not appended) because
	// they use system role, and Claude API requires system messages at the start.
	messages = h.prependAttachments(ctx, sid, messages)

	// Normalize messages for LLM: extract system messages to the front,
	// repair tool call/result pairing, merge consecutive same-role messages,
	// and validate structural compliance with the Anthropic Messages API.
	// This is the final defensive pass before messages reach the LLM.
	messages, err = h.normalizeMessagesForLLM(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: normalize: %w", err)
	}

	// Set strategic cache breakpoints to protect stable content from invalidation
	// caused by microcompact or tool result budget modifications.
	// MUST be called AFTER normalizeMessagesForLLM (which reorders system messages).
	if h.enableStrategicCacheBreakpoints {
		messages = h.setCacheBreakpoints(messages)
	}

	// Trigger background Session Memory extraction (async, non-blocking).
	// The extractor checks whether extraction is needed based on token growth
	// and tool call count.
	if len(messages) > 0 {
		h.triggerSessionMemoryExtraction(ctx, sid, messages)
	}

	// Debug logging: record all loaded message previews (Debug level to avoid production noise).
	if len(messages) > 0 {
		var preview []string
		for i, msg := range messages {
			content := msg.Content
			if content == "" && msg.ReasoningContent != "" {
				content = "[thinking]" + msg.ReasoningContent
			}
			if len(content) > 10 {
				content = content[:10] + "..."
			}
			preview = append(preview, fmt.Sprintf("[%d]%s:%s", i, msg.Role, content))
		}
		h.logger.Debug(ctx, "loadMessages.all_messages", map[string]any{
			"session_id":    sid.String(),
			"message_count": len(messages),
			"messages":      preview,
		})
	}

	return messages, nil
}

// prependAttachments builds system-role attachments (TodoList, SessionMemory,
// UserMemory) and prepends them to the message array. If no attachments are
// available or the attachment manager is nil, the original messages are
// returned unchanged.
func (h *helpers) prependAttachments(ctx context.Context, sid uuid.UUID, messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) == 0 || h.attachmentManager == nil {
		return messages
	}

	var userID uuid.UUID
	if session, sessionErr := h.deps.SessionRepo.GetByID(ctx, sid); sessionErr == nil && session != nil {
		if parsedID, parseErr := uuid.Parse(session.OwnerRefID); parseErr == nil {
			userID = parsedID
		}
	}

	attachmentMsgs, err := h.attachmentManager.BuildAttachments(ctx, sid, userID)
	if err != nil {
		h.logger.Warn(ctx, "loadMessages.build_attachments_failed", map[string]any{
			"session_id": sid.String(),
			"error":      err.Error(),
		})
		return messages
	}
	if len(attachmentMsgs) == 0 {
		return messages
	}

	// Prepend attachments so system-role entries appear before user/assistant
	// messages, complying with Claude API requirements.
	return append(attachmentMsgs, messages...)
}

