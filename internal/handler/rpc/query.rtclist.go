package rpchandler

import (
	"context"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// RtcList 获取当前用户会话的 RTC 列表（按创建时间升序，游标分页）。
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
