package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// finalizeStreamMessage forces finalization of a streaming message that may
// not have received a FinishReason chunk. This mirrors the old
// finalizeStreamMessage in stream_message.go.
func (h *helpers) finalizeStreamMessage(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID uuid.UUID,
	msgID *uuid.UUID,
	buildContent func(string) (protocol.ContentData, error),
	kind string,
) error {
	if *msgID == uuid.Nil {
		return nil
	}

	// Use a synthetic empty chunk with a FinishReason to trigger the
	// finalization path in appendStreamChunk.
	return h.appendStreamChunk(ctx, sessionID, turnID, "", "stream_finalize",
		msgID, new(bool), buildContent, kind)
}

// createAndPublishMessage creates a message record and publishes a
// message.created event atomically via RunAndPublish.
//
// Mirrors the old createAndPublishMessage in session_actor.go.
func (h *helpers) createAndPublishMessage(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID uuid.UUID,
	role protocol.MessageRole,
	content protocol.ContentData,
	streamingStatus protocol.MessageStreamingStatus,
	topicCh string,
) (*model.Message, error) {
	var newMsg *model.Message
	_, err := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		var createErr error
		newMsg, createErr = primitives.CreateMessage(
			txCtx, h.deps,
			sessionID, &turnID,
			role,
			usecase.SystemCreator{},
			content,
			streamingStatus,
			"",  // clientID — system-generated
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create message: %w", createErr)
		}

		return []updates.UpdatePublishItem{{
			Channel: topicCh,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionCreated, EntityId: protocol.UUID(newMsg.ID.String())},
			},
		}}, nil
	})
	if err != nil {
		return nil, err
	}
	return newMsg, nil
}

// =============================================================================
// Stream state tracking
// =============================================================================

// turnStreamState tracks the streaming message state for one turn.
// Each turn may have up to two concurrent streams: one for markdown content
// and one for thinking/reasoning content.
type turnStreamState struct {
	markdownMsgID     uuid.UUID
	markdownFinalized bool
	thinkingMsgID     uuid.UUID
	thinkingFinalized bool
}

// appendStreamChunk handles one chunk of a streaming response.
//
// On the first chunk (msgID == uuid.Nil): create a message record with
// streaming_status=streaming, publish message.created, and start buffering
// chunks in Redis.
//
// On subsequent chunks: append to the Redis buffer and publish
// message.updated to the live channel (real-time, no persistence).
//
// On the final chunk (finishReason != ""): read all chunks from Redis,
// concatenate, update the DB with the full content, delete Redis buffer,
// and publish message.updated to the topic channel.
func (h *helpers) appendStreamChunk(
	ctx context.Context,
	sessionID uuid.UUID,
	turnID uuid.UUID,
	chunkContent string,
	finishReason string,
	msgID *uuid.UUID,
	finalized *bool,
	buildContent func(string) (protocol.ContentData, error),
	kind string,
) error {
	if h.deps.UpdatePublisher == nil {
		return nil
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("appendStreamChunk: get session: %w", err)
	}
	topicCh := channel.UserTopic(session.OwnerRefID)
	liveCh := channel.UserLive(session.OwnerRefID)

	isFirst := *msgID == uuid.Nil
	isLast := finishReason != ""

	// First chunk: create message record.
	if isFirst {
		emptyContent, contentErr := buildContent("")
		if contentErr != nil {
			return fmt.Errorf("appendStreamChunk: create empty content: %w", contentErr)
		}
		newMsg, createErr := h.createAndPublishMessage(ctx, sessionID, turnID,
			protocol.MessageRoleAssistant, emptyContent,
			protocol.MessageStreamingStreaming, topicCh)
		if createErr != nil {
			return fmt.Errorf("appendStreamChunk: create message: %w", createErr)
		}
		*msgID = newMsg.ID
	}

	msgIDStr := msgID.String()

	// Append chunk to Redis buffer.
	streamStore := h.getStreamStore()
	if streamStore != nil {
		if _, appendErr := streamStore.AppendChunk(msgIDStr, chunkContent); appendErr != nil {
			h.logIfEnabled(ctx, "appendStreamChunk.redis_append_failed", map[string]any{
				"message_id": msgIDStr,
				"error":      appendErr.Error(),
			})
		}
	}

	// Not last chunk: publish to live channel (real-time update).
	if !isLast {
		if _, pubErr := h.deps.UpdatePublisher.Publish(ctx, updates.UpdatePublishItem{
			Channel: liveCh,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionUpdated, EntityId: protocol.UUID(msgIDStr)},
			},
		}); pubErr != nil {
			h.logIfEnabled(ctx, "appendStreamChunk.live_publish_failed", map[string]any{
				"message_id": msgIDStr,
				"error":      pubErr.Error(),
			})
		}
		return nil
	}

	// Last chunk: finalize.
	// 1. Read all chunks from Redis and concatenate.
	var fullContent string
	if streamStore != nil {
		chunks, readErr := streamStore.GetAllChunks(msgIDStr)
		if readErr != nil {
			return fmt.Errorf("appendStreamChunk: get all chunks: %w", readErr)
		}
		fullContent = strings.Join(chunks, "")
	} else {
		fullContent = chunkContent
	}

	// 2. Serialize final content as ContentData JSON.
	finalContentData, contentErr := buildContent(fullContent)
	if contentErr != nil {
		return fmt.Errorf("appendStreamChunk: build final content: %w", contentErr)
	}
	serializedContent, serializeErr := primitives.SerializeContentData(finalContentData)
	if serializeErr != nil {
		return fmt.Errorf("appendStreamChunk: serialize final content: %w", serializeErr)
	}

	// 3. Update DB + publish message.updated to topic channel.
	if _, pubErr := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := h.deps.MessageRepo.UpdateStreamingStatus(txCtx, *msgID, protocol.MessageStreamingCompleted, serializedContent); err != nil {
			return nil, fmt.Errorf("update streaming status: %w", err)
		}
		return []updates.UpdatePublishItem{{
			Channel: topicCh,
			Items: []protocol.UpdateItem{
				{Entity: protocol.EntityMessage, Action: protocol.ActionUpdated, EntityId: protocol.UUID(msgIDStr)},
			},
		}}, nil
	}); pubErr != nil {
		return fmt.Errorf("appendStreamChunk: run and publish: %w", pubErr)
	}

	// 4. Delete Redis chunks.
	if streamStore != nil {
		if delErr := streamStore.DeleteChunks(msgIDStr); delErr != nil {
			h.logIfEnabled(ctx, "appendStreamChunk.redis_delete_failed", map[string]any{
				"message_id": msgIDStr,
				"error":      delErr.Error(),
			})
		}
	}

	*finalized = true
	return nil
}
