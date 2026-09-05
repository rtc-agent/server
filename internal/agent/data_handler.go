package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Event handlers (internal)
// =============================================================================

// handleStreamChunk processes a single streaming chunk.
//
// For the first chunk of a stream, a new message record is created with
// streaming_status=streaming. For subsequent chunks, the content is appended
// to the Redis buffer. On the final chunk (FinishReason != ""), the message
// is finalized: all chunks are read from Redis, concatenated, and written
// to the DB with streaming_status=completed.
//
// This mirrors the old publishStreamChunkCore in stream_message.go.
func (h *helpers) handleStreamChunk(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	state := h.streamState.getOrCreate(turnID.String())

	// Handle markdown content.
	if event.Content != "" {
		if err := h.appendStreamChunk(ctx, sessionID, turnID, event.Content, event.FinishReason, &state.markdownMsgID, &state.markdownFinalized, primitives.MarkdownContentData, "markdown"); err != nil {
			return err
		}
	}

	// Handle reasoning/thinking content.
	if event.ReasoningContent != "" {
		if err := h.appendStreamChunk(ctx, sessionID, turnID, event.ReasoningContent, event.FinishReason, &state.thinkingMsgID, &state.thinkingFinalized, primitives.ThinkingContentData, "thinking"); err != nil {
			return err
		}
	}

	// When markdown content arrives and thinking hasn't been finalized,
	// finalize the thinking message (thinking and content are mutually
	// exclusive phases).
	if event.Content != "" && state.thinkingMsgID != uuid.Nil && !state.thinkingFinalized {
		if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.thinkingMsgID, primitives.ThinkingContentData, "thinking"); err != nil {
			return err
		}
		state.thinkingFinalized = true
	}

	// Finalize markdown if FinishReason is set.
	// IMPORTANT: If FinishReason is set but Content is empty, we still need to
	// call appendStreamChunk (via finalizeStreamMessage) to trigger the finalization
	// path that updates the DB status to "completed".
	if event.FinishReason != "" && state.markdownMsgID != uuid.Nil && !state.markdownFinalized {
		if event.Content == "" {
			// No content in this chunk, but we need to finalize the stream
			if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.markdownMsgID, primitives.MarkdownContentData, "markdown"); err != nil {
				return err
			}
		}
		state.markdownFinalized = true
	}
	if event.FinishReason != "" && state.thinkingMsgID != uuid.Nil && !state.thinkingFinalized {
		if event.ReasoningContent == "" {
			// No reasoning content in this chunk, but we need to finalize the stream
			if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.thinkingMsgID, primitives.ThinkingContentData, "thinking"); err != nil {
				return err
			}
		}
		state.thinkingFinalized = true
	}

	return nil
}

// handleStreamEnd processes a stream-end event.
//
// Finalizes any pending streaming messages that haven't received a
// FinishReason chunk (e.g., thinking messages that ended when content
// started arriving).
func (h *helpers) handleStreamEnd(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	state := h.streamState.getOrCreate(turnID.String())

	// Finalize thinking if pending.
	if state.thinkingMsgID != uuid.Nil && !state.thinkingFinalized {
		if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.thinkingMsgID, primitives.ThinkingContentData, "thinking"); err != nil {
			return err
		}
		state.thinkingFinalized = true
	}

	// Finalize markdown if pending.
	if state.markdownMsgID != uuid.Nil && !state.markdownFinalized {
		if err := h.finalizeStreamMessage(ctx, sessionID, turnID, &state.markdownMsgID, primitives.MarkdownContentData, "markdown"); err != nil {
			return err
		}
		state.markdownFinalized = true
	}

	// Clean up per-turn state.
	h.streamState.remove(turnID.String())

	return nil
}

// handleMessage processes a complete, non-streaming message.
//
// For assistant messages: if the message has ReasoningContent, create a
// thinking message first, then create the markdown message.
//
// For tool messages: skip (tool messages are handled by the tool itself).
//
// Batching: when both thinking and main messages are created, they are created
// in a single RunAndPublish transaction with a single batch centrifuge publish,
// reducing Redis offset increments, DB round-trips, and centrifuge calls from
// 2 transactions to 1.
func (h *helpers) handleMessage(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	if event.Message == nil {
		return nil
	}

	// Skip tool messages — they are created by the tool itself.
	if event.Message.Role == string(schema.Tool) {
		return nil
	}

	if h.deps.UpdatePublisher == nil {
		return nil
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("handleMessage: get session: %w", err)
	}
	ch := channel.UserTopic(session.OwnerRefID)

	hasThinking := event.Message.ReasoningContent != ""

	// Prepare content for both message types up front (before the transaction).
	var thinkingContent protocol.ContentData
	if hasThinking {
		thinkingContent, err = primitives.ThinkingContentData(event.Message.ReasoningContent)
		if err != nil {
			return fmt.Errorf("handleMessage: create thinking content: %w", err)
		}
	}
	markdownContent, err := primitives.MarkdownContentData(event.Message.Content)
	if err != nil {
		return fmt.Errorf("handleMessage: create markdown content: %w", err)
	}

	// Build the list of messages to create in a single batch transaction.
	messagesToCreate := make([]primitives.MessageToCreate, 0, 2)
	if hasThinking {
		messagesToCreate = append(messagesToCreate, primitives.MessageToCreate{
			Role:    protocol.MessageRoleAssistant,
			Creator: usecase.SystemCreator{},
			Content: thinkingContent,
			Status:  protocol.MessageStreamingCompleted,
		})
	}
	messagesToCreate = append(messagesToCreate, primitives.MessageToCreate{
		Role:    protocol.MessageRoleAssistant,
		Creator: usecase.SystemCreator{},
		Content: markdownContent,
		Status:  protocol.MessageStreamingCompleted,
	})

	// Single RunAndPublish: create all messages in one DB batch and publish
	// all message.created events in one centrifuge call.
	_, err = h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		createdMsgs, createErr := primitives.BatchCreateMessages(txCtx, h.deps, sessionID, &turnID, messagesToCreate)
		if createErr != nil {
			return nil, fmt.Errorf("batch create messages: %w", createErr)
		}

		// Build one UpdatePublishItem with all message.created events merged.
		var allItems []protocol.UpdateItem
		for _, msg := range createdMsgs {
			allItems = append(allItems, protocol.UpdateItem{
				Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(msg.ID.String()),
			})
		}
		return []updates.UpdatePublishItem{{Channel: ch, Items: allItems}}, nil
	})
	if err != nil {
		return fmt.Errorf("handleMessage: %w", err)
	}

	return nil
}

// handleEventError processes an event-level error.
//
// These are errors attached to a single event in the agent's output stream,
// not turn-level failures (which go through FailTurn). The typical action
// is to log the error and continue; the turn may still complete successfully.
func (h *helpers) handleEventError(ctx context.Context, sessionID uuid.UUID, turnID uuid.UUID, event *turnagent.Event) error {
	errMsg := ""
	if event.Err != nil {
		errMsg = event.Err.Error()
	}
	h.logIfEnabled(ctx, "handleEventError", map[string]any{
		"session_id": sessionID.String(),
		"turn_id":    turnID.String(),
		"error":      errMsg,
	})
	return nil
}
