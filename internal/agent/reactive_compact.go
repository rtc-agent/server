package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// recoverFromPromptTooLong is the RecoverFromPromptTooLong callback.
//
// Strategy (escalating by attempt number, passed in by the caller):
//   - Attempt 1: Aggressive microcompact (clear all but 1 tool result) + force compact.
//   - Attempt 2: More aggressive compact (MinTokens=500, MinTextBlockMessages=1).
//   - Attempt 3: Soft-delete the oldest half of non-summary messages from DB.
//
// Each step persists the result so the next TurnLoop's loadMessages picks up
// the compressed state.
func (h *helpers) recoverFromPromptTooLong(ctx context.Context, sessionID string, attempt int) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("reactive compact: invalid session ID %q: %w", sessionID, err)
	}

	h.logIfEnabled(ctx, "reactive_compact.start", map[string]any{
		"session_id": sessionID,
		"attempt":    attempt,
	})

	switch attempt {
	case 1:
		// Level 1: Aggressive microcompact + force compact.
		return h.reactiveCompactLevel1(ctx, sid)
	case 2:
		// Level 2: More aggressive compact with minimal retention.
		return h.reactiveCompactLevel2(ctx, sid)
	default:
		// Level 3+: Soft-delete oldest messages from DB.
		return h.reactiveCompactLevel3(ctx, sid)
	}
}

// reactiveCompactLevel1: Clear old tool results aggressively, then force
// summarize the remaining messages. This reuses the existing compression
// pipeline (summarizeMessages → logSummarizeTokenUsage → persistCompressedMessages).
func (h *helpers) reactiveCompactLevel1(ctx context.Context, sessionID uuid.UUID) error {
	// 1. Load messages from DB (applies normal budget + microcompact).
	messages, err := h.loadMessages(ctx, sessionID.String())
	if err != nil {
		return fmt.Errorf("reactive compact L1: load messages: %w", err)
	}

	// 2. Apply aggressive microcompact: keep only 1 tool result.
	messages = aggressiveMicrocompact(messages, 1)

	// 3. Convert to schema.Message for compression.
	schemaMsgs := turnagent.MessagesToEino(messages)

	// 4. Force compress (bypasses token threshold check).
	compressed, err := h.forceCompressContext(ctx, schemaMsgs, aggressiveRetentionConfig())
	if err != nil {
		return fmt.Errorf("reactive compact L1: compress: %w", err)
	}

	// 5. Persist the summary.
	return h.persistCompressedMessages(ctx, compressed)
}

// reactiveCompactLevel2: Even more aggressive compression — minimal retention.
func (h *helpers) reactiveCompactLevel2(ctx context.Context, sessionID uuid.UUID) error {
	// 1. Load messages from DB.
	messages, err := h.loadMessages(ctx, sessionID.String())
	if err != nil {
		return fmt.Errorf("reactive compact L2: load messages: %w", err)
	}

	// 2. Aggressive microcompact: keep 0 tool results (clear all).
	messages = aggressiveMicrocompact(messages, 0)

	// 3. Convert to schema.Message.
	schemaMsgs := turnagent.MessagesToEino(messages)

	// 4. Force compress with minimal retention.
	compressed, err := h.forceCompressContext(ctx, schemaMsgs, minimalRetentionConfig())
	if err != nil {
		return fmt.Errorf("reactive compact L2: compress: %w", err)
	}

	// 5. Persist the summary.
	return h.persistCompressedMessages(ctx, compressed)
}

// reactiveCompactLevel3: Soft-delete the oldest half of non-summary messages
// from the DB. No LLM call needed — this is a pure data operation.
func (h *helpers) reactiveCompactLevel3(ctx context.Context, sessionID uuid.UUID) error {
	// 1. Load messages from DB.
	dbMsgs, err := h.deps.MessageRepo.ListRecentBySession(ctx, sessionID, 200)
	if err != nil {
		return fmt.Errorf("reactive compact L3: list messages: %w", err)
	}

	// 2. Find the most recent summary message to determine what's already compressed.
	summaryIdx := -1
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		contentData, parseErr := primitives.ParseContentData(dbMsgs[i].Content)
		if parseErr == nil && contentData.Type == protocol.ContentTypeSummary {
			summaryIdx = i
			break
		}
	}

	// 3. Collect messages after the summary (these are the "live" messages).
	var liveMsgs []*model.Message
	startIdx := 0
	if summaryIdx >= 0 {
		startIdx = summaryIdx + 1
	}
	for i := startIdx; i < len(dbMsgs); i++ {
		liveMsgs = append(liveMsgs, dbMsgs[i])
	}

	if len(liveMsgs) <= 2 {
		// Nothing meaningful to delete.
		return fmt.Errorf("reactive compact L3: only %d live messages remain, cannot reduce further", len(liveMsgs))
	}

	// 4. Soft-delete the oldest half of live messages.
	deleteCount := len(liveMsgs) / 2
	idsToDelete := make([]uuid.UUID, 0, deleteCount)
	for i := 0; i < deleteCount; i++ {
		idsToDelete = append(idsToDelete, liveMsgs[i].ID)
	}

	if err := h.deps.MessageRepo.DeleteByIDs(ctx, idsToDelete); err != nil {
		return fmt.Errorf("reactive compact L3: soft-delete messages: %w", err)
	}

	h.logIfEnabled(ctx, "reactive_compact.L3.deleted", map[string]any{
		"session_id":  sessionID.String(),
		"deleted":     len(idsToDelete),
		"remaining":   len(liveMsgs) - len(idsToDelete),
	})

	return nil
}

// aggressiveMicrocompact clears old tool results, keeping only the most recent
// `keepRecent` compactable tool results. Unlike the time-based microcompact,
// this does NOT check the idle time gap — it always applies.
//
// If keepRecent is 0, ALL compactable tool results are cleared.
func aggressiveMicrocompact(messages []*turnagent.Message, keepRecent int) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	// Collect all compactable tool call IDs in order.
	compactableIDs := collectCompactableToolCallIDs(messages)
	if len(compactableIDs) == 0 {
		return messages
	}

	// Determine which IDs to keep.
	if keepRecent >= len(compactableIDs) {
		return messages // nothing to clear
	}

	keepSet := make(map[string]struct{})
	if keepRecent > 0 {
		for _, id := range compactableIDs[len(compactableIDs)-keepRecent:] {
			keepSet[id] = struct{}{}
		}
	}
	// If keepRecent == 0, keepSet is empty → all tool results are cleared.

	result := make([]*turnagent.Message, len(messages))
	for i, msg := range messages {
		if msg.Role == turnagent.RoleTool && msg.ToolCallID != "" {
			if _, kept := keepSet[msg.ToolCallID]; !kept {
				result[i] = &turnagent.Message{
					Role:       msg.Role,
					Content:    TIME_BASED_MC_CLEARED_MESSAGE,
					ToolName:   msg.ToolName,
					ToolCallID: msg.ToolCallID,
					CreatedAt:  msg.CreatedAt,
				}
				continue
			}
		}
		result[i] = msg
	}
	return result
}

// aggressiveRetentionConfig returns a retention config that keeps fewer messages
// than the default, suitable for reactive compact Level 1.
func aggressiveRetentionConfig() RetentionConfig {
	return RetentionConfig{
		MinTokens:            2000,
		MinTextBlockMessages: 2,
		MaxTokens:            10000,
	}
}

// minimalRetentionConfig returns the most aggressive retention config —
// keep as few messages as possible. Suitable for reactive compact Level 2.
func minimalRetentionConfig() RetentionConfig {
	return RetentionConfig{
		MinTokens:            500,
		MinTextBlockMessages: 1,
		MaxTokens:            5000,
	}
}

// forceCompressContext forces compression of messages using the given retention
// config. Unlike compressContext, it:
//   - Skips the shouldCompressByTokenCount check (always compresses).
//   - Uses the provided aggressive retention config.
//   - If retentionIndex >= len(msgs) (all fit in budget), forces a full compact.
//
// This reuses the existing summarizeMessages pipeline, which means
// logSummarizeTokenUsage is called for the summarization LLM call.
func (h *helpers) forceCompressContext(ctx context.Context, msgs []*schema.Message, config RetentionConfig) ([]*schema.Message, error) {
	if len(msgs) < 2 {
		return msgs, nil
	}

	retentionIndex := calculateRetentionIndex(msgs, config)

	// If all messages fit in the aggressive retention budget, force a full
	// compact — the LLM rejected the prompt, so we must reduce somehow.
	if retentionIndex >= len(msgs) {
		retentionIndex = 0
	}

	var (
		summary string
		err     error
	)

	if retentionIndex == 0 {
		summary, err = h.summarizeMessages(ctx, msgs, CompactModeFull, nil)
	} else {
		summary, err = h.summarizeMessages(ctx, msgs[:retentionIndex], CompactModePartial, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("force compress: summarize: %w", err)
	}

	summaryContent := formatCompactUserMessage(formatCompactSummary(summary))
	summaryMsg := &schema.Message{
		Role:    schema.User,
		Content: summaryContent,
	}

	// Preserve system messages from the discarded portion (same rationale as
	// compressContext): they carry dynamic attachments (AgentPrompt, TodoList,
	// etc.) that must survive compression.
	var systemMsgs []*schema.Message
	for _, msg := range msgs[:retentionIndex] {
		if msg.Role == schema.System {
			systemMsgs = append(systemMsgs, msg)
		}
	}

	result := make([]*schema.Message, 0, 1+len(systemMsgs)+len(msgs)-retentionIndex)
	result = append(result, summaryMsg)
	result = append(result, systemMsgs...)
	if retentionIndex < len(msgs) {
		result = append(result, msgs[retentionIndex:]...)
	}

	return result, nil
}
