package rpchandler

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
)

// RtcGet 获取当前用户会话中的单个 RTC。
func (h *Handler) RtcGet(ctx context.Context, req *protocol.RtcGetRequest) (*protocol.RtcGetResponse, error) {
	userID, err := h.requireUserID(ctx)
	if err != nil {
		return nil, err
	}

	rtcUUID, apiErr := parseUUID(req.RtcId, "rtc_id")
	if apiErr != nil {
		return nil, apiErr
	}

	rtc, err := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{
				Code:    "rtc.not_found",
				Message: fmt.Sprintf("rtc %s not found", req.RtcId),
			}
		}
		return nil, &APIError{Code: "rtc.error", Message: err.Error()}
	}

	if _, err := h.loadOwnedSession(ctx, rtc.SessionID, userID); err != nil {
		return nil, err
	}

	logger.Info("[RtcGet] user=%s rtc=%s", userID, req.RtcId)
	return &protocol.RtcGetResponse{
		Item: model.ToProtocolRtc(rtc),
	}, nil
}
