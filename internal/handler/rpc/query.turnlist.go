//nolint:dupl // structurally similar to RtcList but operates on different entity/repo pair
package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnList retrieves the Turn list for the current user's session (ordered by creation time ascending, cursor pagination).
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
