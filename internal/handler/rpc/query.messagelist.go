package rpchandler

import (
	"context"
	"fmt"
	"strconv"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// MessageList retrieves the message list for the current user's session (ordered by global_offset ascending, cursor pagination).
func (h *Handler) MessageList(ctx context.Context, req *protocol.MessageListRequest) (*protocol.MessageListResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

	limit, apiErr := clampLimit(req.Limit, h.deps.API.QueryDefaultLimit, h.deps.API.QueryMaxLimit)
	if apiErr != nil {
		return nil, apiErr
	}

	sessionUUID, apiErr := parseUUID(req.SessionId, "session_id")
	if apiErr != nil {
		return nil, apiErr
	}

	if _, err := h.loadOwnedSession(ctx, sessionUUID, userID); err != nil {
		return nil, err
	}

	// Parse string cursor to uint32 (global_offset).
	var cursor *uint32
	if req.Cursor != nil && *req.Cursor != "" {
		offset, parseErr := strconv.ParseUint(*req.Cursor, 10, 32)
		if parseErr != nil {
			return nil, &APIError{
				Code:    ErrorCodeMessageInvalidCursor,
				Message: fmt.Sprintf("invalid cursor value: %s", *req.Cursor),
			}
		}
		offset32 := uint32(offset)
		cursor = &offset32
	}

	messages, err := h.deps.Deps.MessageRepo.ListBySession(ctx, sessionUUID, cursor, limit)
	if err != nil {
		return nil, h.internalError(ctx, "message.list_failed", "internal error", err)
	}

	items := make([]protocol.Message, 0, len(messages))
	for _, m := range messages {
		items = append(items, model.ToProtocolMessage(m))
	}

	var nextCursor *string
	if len(messages) == limit {
		last := fmt.Sprintf("%d", messages[len(messages)-1].GlobalOffset)
		nextCursor = &last
	}

	logger.Info(ctx, "[MessageList]",
		zap.String("user", userID.String()),
		zap.String("session", req.SessionId),
		zap.Int("count", len(items)),
		zap.Bool("has_next", nextCursor != nil))
	return &protocol.MessageListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
