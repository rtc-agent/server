package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/dbmodel"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnList 获取当前用户会话的 Turn 列表（按创建时间升序，游标分页）。
func (h *Handler) TurnList(ctx context.Context, req *protocol.TurnListRequest) (*protocol.TurnListResponse, error) {
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

	turns, err := h.deps.Deps.TurnRepo.ListBySession(ctx, sessionUUID, req.Cursor, limit)
	if err != nil {
		return nil, &APIError{Code: "turn.list_failed", Message: err.Error()}
	}

	items := make([]protocol.Turn, 0, len(turns))
	for _, t := range turns {
		items = append(items, dbmodel.ToProtocolTurn(t))
	}

	var nextCursor *string
	if len(turns) == limit {
		last := turns[len(turns)-1].ID.String()
		nextCursor = &last
	}

	logger.Info("[TurnList] user=%s session=%s count=%d has_next=%v", userID, req.SessionId, len(items), nextCursor != nil)
	return &protocol.TurnListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
