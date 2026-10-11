//nolint:dupl // structurally similar to RtcList but operates on different entity/repo pair
package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/converter"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnList retrieves the Turn list for the current user's session (ordered by creation time ascending, cursor pagination).
func (h *Handler) TurnList(ctx context.Context, req *protocol.TurnListRequest) (*protocol.TurnListResponse, error) {
	items, nextCursor, err := listBySessionCursor(ctx, h,
		req.SessionId, req.Limit, req.Cursor, "turn",
		h.deps.Deps.RtcSessionUsecase.ListTurnsBySession,
		converter.ToProtocolTurn,
		func(t *converter.Turn) string { return t.ID.String() },
	)
	if err != nil {
		return nil, err
	}
	return &protocol.TurnListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
