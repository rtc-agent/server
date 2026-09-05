package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/schema"
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

	const historyLimit = 200
	dbMsgs, err := h.deps.MessageRepo.ListRecentBySession(ctx, sid, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: list messages for session %s: %w", sessionID, err)
	}

	// Find the most recent summary message (if any) and truncate the history
	// to start from it. Messages before the summary are already compressed
	// into it and should not be sent to the agent.
	//
	// ListRecentBySession returns messages in ASC order (oldest first within
	// the returned set), so we iterate backwards to find the first summary.
	tmpMsgs := make([]*model.Message, 0, len(dbMsgs))
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		msg := dbMsgs[i]
		tmpMsgs = append(tmpMsgs, msg)
		contentData, parseErr := primitives.ParseContentData(msg.Content)
		if parseErr == nil && contentData.Type == protocol.ContentTypeSummary {
			break
		}
	}
	// Reverse to get chronological order (oldest first).
	sort.Slice(tmpMsgs, func(i, j int) bool {
		return tmpMsgs[i].GlobalOffset < tmpMsgs[j].GlobalOffset
	})
	dbMsgs = tmpMsgs

	// Convert DB messages to turn-agent Messages.
	// The conversion logic mirrors the old SchemaMessages method in context.go,
	// but produces turnagent.Message instead of schema.Message.
	var messages []*turnagent.Message
	for _, msg := range dbMsgs {
		converted := convertDBMessage(msg)
		messages = append(messages, converted...)
	}

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
		return convertSummaryContent(contentData.Data)

	case protocol.ContentTypeText, protocol.ContentTypeMarkdown:
		text, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:       msg.Role,
			Content:    text,
			TokenUsage: tokenUsage,
		}}

	case protocol.ContentTypeThinking:
		tk, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:             msg.Role,
			ReasoningContent: tk,
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
func convertSummaryContent(data any) []*turnagent.Message {
	dataBytes, err := primitives.ContentDataBytes(data)
	if err != nil {
		return nil
	}
	var items []primitives.SummaryItem
	if err := json.Unmarshal(dataBytes, &items); err != nil {
		return nil
	}
	var msgs []*turnagent.Message
	for _, item := range items {
		msgs = append(msgs, &turnagent.Message{
			Role:    item.Role,
			Content: item.Content,
		})
	}
	return msgs
}
