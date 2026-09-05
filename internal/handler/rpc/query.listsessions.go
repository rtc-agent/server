package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// ListSessions 默认分页大小（最大复用 queryMaxLimit）。
const listSessionsDefaultLimit = 20

// ListSessions 获取当前用户的会话列表（按创建时间倒序，游标分页）。
// RPC 专属查询，不共享给 LLM，业务逻辑直接实现在此。
func (h *Handler) ListSessions(ctx context.Context, req *protocol.ListSessionsRequest) (*protocol.ListSessionsResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

	limit, apiErr := clampLimit(req.Limit, listSessionsDefaultLimit, h.deps.API.QueryMaxLimit)
	if apiErr != nil {
		return nil, apiErr
	}

	sessions, err := h.deps.SessionRepo.GetByUser(ctx, userID, req.Cursor, limit)
	if err != nil {
		return nil, h.internalError(ctx, "session.list_failed", "internal error", err)
	}

	items := make([]protocol.Session, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, model.ToProtocolSession(s))
	}

	// 当返回数量等于 limit 时，认为可能还有下一页，以最后一条 ID 作为游标
	var nextCursor *string
	if len(sessions) == limit {
		last := sessions[len(sessions)-1].ID.String()
		nextCursor = &last
	}

	logger.Info(ctx, "[ListSessions]",
		zap.String("user", userID.String()),
		zap.Int("count", len(items)),
		zap.Bool("has_next", nextCursor != nil))
	return &protocol.ListSessionsResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
