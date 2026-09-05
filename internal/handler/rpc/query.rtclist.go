package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// RtcList 获取当前用户会话的 RTC 列表（按创建时间升序，游标分页）。
func (h *Handler) RtcList(ctx context.Context, req *protocol.RtcListRequest) (*protocol.RtcListResponse, error) {
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

	rtcs, err := h.deps.Deps.RtcRepo.ListBySession(ctx, sessionUUID, req.Cursor, limit)
	if err != nil {
		return nil, h.internalError(ctx, "rtc.list_failed", "internal error", err)
	}

	items := make([]protocol.Rtc, 0, len(rtcs))
	for _, r := range rtcs {
		items = append(items, model.ToProtocolRtc(r))
	}

	var nextCursor *string
	if len(rtcs) == limit {
		last := rtcs[len(rtcs)-1].ID.String()
		nextCursor = &last
	}

	logger.Info(ctx, "[RtcList]",
		zap.String("user", userID.String()),
		zap.String("session", string(req.SessionId)),
		zap.Int("count", len(items)),
		zap.Bool("has_next", nextCursor != nil))
	return &protocol.RtcListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
