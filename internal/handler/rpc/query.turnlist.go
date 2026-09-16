package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnList 获取当前用户会话的 Turn 列表（按创建时间升序，游标分页）。
func (h *Handler) TurnList(ctx context.Context, req *protocol.TurnListRequest) (*protocol.TurnListResponse, error) {
	items, nextCursor, err := listBySessionCursor(ctx, h,
		req.SessionId, req.Limit, req.Cursor, "turn",
		h.deps.Deps.TurnRepo.ListBySession,
		model.ToProtocolTurn,
		func(t *model.Turn) string { return t.ID.String() },
	)
	if err != nil {
		return nil, err
	}
	return &protocol.TurnListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
