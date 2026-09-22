package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/stringutil"
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
	//
	// Exception: prompt-type messages are always preserved regardless of
	// position, because they contain persistent system instructions that
	// must survive compaction (design principle: "系统提示词应该一直存在于上下文中").
	summaryIdx := -1
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		contentData, parseErr := primitives.ParseContentData(dbMsgs[i].Content)
		if parseErr == nil && contentData.Type == protocol.ContentTypeSummary {
			summaryIdx = i
			break
		}
	}

	if summaryIdx > 0 {
		// Collect prompt messages that are before the summary boundary.
		// These would be lost by truncation but must be preserved.
		var preservedPrompts []*model.Message
		for i := 0; i < summaryIdx; i++ {
			contentData, parseErr := primitives.ParseContentData(dbMsgs[i].Content)
			if parseErr == nil && contentData.Type == protocol.ContentTypePrompt {
				preservedPrompts = append(preservedPrompts, dbMsgs[i])
			}
		}
		// Truncate to summary boundary, then prepend preserved prompt messages.
		// preservedPrompts are in global_offset order (scanned left to right).
		dbMsgs = append(preservedPrompts, dbMsgs[summaryIdx:]...)
	} else if summaryIdx == 0 {
		// Summary is the first message — keep all messages from summary onward.
		// No messages before summary to preserve.
		dbMsgs = dbMsgs[summaryIdx:]
	}
	// If summaryIdx < 0, no summary found — keep all messages as-is.

	// Convert DB messages to turn-agent Messages.
	// The conversion logic mirrors the old SchemaMessages method in context.go,
	// but produces turnagent.Message instead of schema.Message.
	//
	// Prompt messages are included at their natural position (based on global_offset).
	// Deduplication is handled by persistCommandPromptsIfNeeded when creating new prompts.
	//
	// convertDBMessage may return nil for unparseable or unrecognized content
	// types; skip those to prevent nil entries from reaching the LLM adapter
	// (which would produce nil schema.Message entries and risk a panic).
	var messages []*turnagent.Message
	var droppedCount int
	for _, msg := range dbMsgs {
		converted, convErr := convertDBMessage(msg)
		if convErr != nil {
			// Log parse errors at Debug level for observability. The overall
			// drop count is logged at Warn below; this adds per-message detail.
			h.logger.Debug(ctx, "loadMessages.convert_failed", map[string]any{
				"session_id": sid.String(),
				"message_id": msg.ID.String(),
				"error":      convErr.Error(),
			})
		}
		if len(converted) == 0 {
			// Only count as dropped if there was a parse error.
			// Empty conversions (e.g., unrecognized content types) are skipped.
			if convErr != nil {
				droppedCount++
			}
			continue
		}
		for _, cm := range converted {
			if cm != nil {
				messages = append(messages, cm)
			}
		}
	}
	// Log dropped messages for observability. Without this, data loss from
	// malformed content is invisible — operators cannot diagnose why the LLM
	// sees fewer messages than expected.
	if droppedCount > 0 {
		h.logger.Warn(ctx, "loadMessages.dropped_unparseable", map[string]any{
			"session_id":    sid.String(),
			"dropped_count": droppedCount,
			"total_db_msgs": len(dbMsgs),
		})
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
	// message, update per-session activation state, and inject the
	// commands' prompt contributions. The /goal command is now handled by
	// GoalWorkflow registered in the registry (see goal_workflow.go).
	//
	// Injection timing: command prompts and scenario prompts are injected
	// BEFORE attachments are prepended, so the final message order is:
	//   [system] Attachments → [system] Scenarios → [system] Command prompts → [conversation]
	// (Attachments win the front position because they are prepended last.)
	//
	// Prompt messages (e.g., goal prompts) are persisted to DB and included
	// in the conversation history by convertDBMessage at their natural position.
	//
	// IMPORTANT: Skip command detection on checkpoint resume (genResume) to avoid
	// re-detecting commands and persisting duplicate prompt messages. The command
	// was already detected and persisted on the original genInput call.
	loadSource := turnagent.LoadSourceFromContext(ctx)
	if loadSource != turnagent.LoadSourceGenResume {
		messages, dbMsgs = h.injectCommandPrompts(ctx, sid, messages, dbMsgs)
	} else {
		h.logger.Debug(ctx, "loadMessages.skip_command_detection", map[string]any{
			"session_id":  sid.String(),
			"load_source": string(loadSource),
		})
	}

	// Inject scenario prompts from the last user message's scenarios field.
	// Pass the already-loaded dbMsgs to avoid a redundant DB query.
	// Note: if scenarios have been persisted as prompt messages (see Phase 2),
	// this function detects them and skips injection to avoid duplicates.
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
	messages, err = h.normalizeMessagesForLLM(ctx, sid, messages)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: normalize: %w", err)
	}

	// Filter out tool results for pending tool calls (checkpoint resume only).
	//
	// On checkpoint resume (genResume), the eino framework's ToolNode will re-invoke
	// the interrupted tool's InvokableRun, which returns the result and causes the
	// ToolNode to create a tool result message. If we also load the toolcall_output
	// from DB, we get duplicate tool_result messages with the same tool_call_id.
	//
	// Instead of skipping ALL toolcall_output (which loses non-RTC tool results),
	// we skip only the tool results whose tool_call_id matches a pending tool call
	// in the checkpoint. Pending IDs are extracted by the HistoryModifier from the
	// checkpoint's State.Messages and passed via context.
	//
	// Filtering happens AFTER normalizeMessagesForLLM so that repairToolPairing
	// correctly validates the full message set before we surgically remove pending
	// results. The assistant messages with pending tool calls are preserved because
	// repairToolPairing already ran and validated them.
	pendingIDs := turnagent.PendingToolCallIDsFromContext(ctx)
	if len(pendingIDs) > 0 {
		filtered := make([]*turnagent.Message, 0, len(messages))
		var skippedCount int
		for _, msg := range messages {
			if msg.Role == turnagent.RoleTool && msg.ToolCallID != "" && pendingIDs[msg.ToolCallID] {
				skippedCount++
				continue
			}
			filtered = append(filtered, msg)
		}
		if skippedCount > 0 {
			// Verification: check if any pending tool results remain after filtering.
			// This should be 0; if > 0, it indicates the eino framework behavior has
			// changed or there's a bug in extractPendingToolCallIDs.
			var residualCount int
			for _, msg := range filtered {
				if msg.Role == turnagent.RoleTool && msg.ToolCallID != "" && pendingIDs[msg.ToolCallID] {
					residualCount++
				}
			}

			h.logger.Info(ctx, "loadMessages.filtered_pending_tool_results", map[string]any{
				"session_id":     sid.String(),
				"skipped_count":  skippedCount,
				"pending_ids":    len(pendingIDs),
				"residual_count": residualCount,
			})

			if residualCount > 0 {
				h.logger.Error(ctx, "loadMessages.pending_filter_verification_failed", map[string]any{
					"session_id":     sid.String(),
					"residual_count": residualCount,
					"skipped_count":  skippedCount,
					"pending_ids":    len(pendingIDs),
					"message":        "Pending tool results still present after filtering - eino framework behavior may have changed or extractPendingToolCallIDs has a bug",
				})
			}
		}
		messages = filtered
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
	//
	// IMPORTANT: Skip extraction on checkpoint resume (genResume) to avoid
	// redundant LLM calls. The extraction was already triggered on the original
	// genInput call for this turn. Resume is just continuing the same turn,
	// not a new conversation point that warrants re-extraction. Extracting on
	// resume wastes tokens without adding value (the token growth threshold
	// already accounts for the work done before interrupt).
	if len(messages) > 0 && loadSource != turnagent.LoadSourceGenResume {
		h.triggerSessionMemoryExtraction(ctx, sid, messages)
	} else if len(messages) > 0 {
		h.logger.Debug(ctx, "loadMessages.skip_session_memory_extraction", map[string]any{
			"session_id":  sid.String(),
			"load_source": string(loadSource),
		})
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
				content = stringutil.TruncateByByte(content, 10)
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
	} else if sessionErr != nil {
		// Log at Warn level so operators can detect session lookup failures
		// that silently cause attachments to be skipped. Without attachments,
		// the LLM operates without persistent context (TodoList, SessionMemory,
		// UserMemory), degrading response quality.
		h.logger.Warn(ctx, "prependAttachments.load_session_failed", map[string]any{
			"session_id": sid.String(),
			"error":      sessionErr.Error(),
			"message":    "session lookup failed; attachments will be skipped",
		})
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
