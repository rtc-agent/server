//nolint:dupl // structurally similar to TurnList but operates on different entity/repo pair
package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// RtcList retrieves the RTC list for the current user's session (ordered by creation time ascending, cursor pagination).
func (h *Handler) RtcList(ctx context.Context, req *protocol.RtcListRequest) (*protocol.RtcListResponse, error) {
	items, nextCursor, err := listBySessionCursor(ctx, h,
		req.SessionId, req.Limit, req.Cursor, "rtc",
		h.deps.Deps.RtcRepo.ListBySession,
		model.ToProtocolRtc,
		func(r *model.Rtc) string { return r.ID.String() },
	)
	if err != nil {
		return nil, err
	}
	return &protocol.RtcListResponse{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}
