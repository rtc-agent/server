package rpchandler

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// RtcGet retrieves a single RTC in the current user's session.
func (h *Handler) RtcGet(ctx context.Context, req *protocol.RtcGetRequest) (*protocol.RtcGetResponse, error) {
	rtc, _, err := getOwnedByID(
		ctx,
		h,
		req.RtcId,
		"rtc_id",
		"rtc.not_found",
		"rtc",
		h.deps.Deps.RtcRepo.GetByID,
		func(r *model.Rtc) uuid.UUID { return r.SessionID },
	)
	if err != nil {
		return nil, err
	}
	return &protocol.RtcGetResponse{
		Item: model.ToProtocolRtc(rtc),
	}, nil
}
