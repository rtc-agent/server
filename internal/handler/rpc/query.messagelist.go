package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// MessageList 获取当前用户会话的消息列表（按 global_offset 升序，游标分页）。
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

	messages, err := h.deps.Deps.MessageRepo.ListBySession(ctx, sessionUUID, req.Cursor, limit)
	if err != nil {
		return nil, h.internalError(ctx, "message.list_failed", "internal error", err)
	}

	items := make([]protocol.Message, 0, len(messages))
	for _, m := range messages {
		items = append(items, model.ToProtocolMessage(m))
	}

	var nextCursor *uint32
	if len(messages) == limit {
		last := messages[len(messages)-1].GlobalOffset
		nextCursor = &last
	}

	logger.Info(ctx, "[MessageList]",
		zap.String("user", userID.String()),
		zap.String("session", string(req.SessionId)),
		zap.Int("count", len(items)),
		zap.Bool("has_next", nextCursor != nil))
	return &protocol.MessageListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
