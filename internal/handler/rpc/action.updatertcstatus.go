// internal/rpchandler/action.updatertcstatus.go
package rpchandler

import (
	"context"
	"errors"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	"go.uber.org/zap"
)

// UpdateRtcStatus 更新 RTC 执行状态（executing/failed/timeout/rejected）。
func (h *Handler) UpdateRtcStatus(ctx context.Context, req *protocol.UpdateRtcStatusRequest) (*protocol.UpdateRtcStatusResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	rtcUUID, apiErr := parseUUID(req.RtcId, "rtc_id")
	if apiErr != nil {
		return nil, apiErr
	}

	logger.Info(ctx, "[UpdateRtcStatus]",
		zap.String("user", userID.String()),
		zap.String("rtc", string(req.RtcId)),
		zap.String("status", string(req.Status)))

	// 校验目标状态合法性
	switch req.Status {
	case protocol.RtcStatusExecuting, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		// valid
	default:
		return nil, &APIError{
			Code:    "rtc.invalid_status",
			Message: fmt.Sprintf("invalid target status: %s", req.Status),
		}
	}

	// 加载 RTC 并校验存在
	rtc, err := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "rtc.not_found", Message: fmt.Sprintf("rtc %s not found", req.RtcId)}
		}
		return nil, h.internalError(ctx, "rtc.error", "internal error", err)
	}

	// 状态校验：终态不允许再变更（幂等：目标状态与当前一致时直接返回成功）
	switch protocol.RtcStatus(rtc.Status) {
	case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		if protocol.RtcStatus(rtc.Status) == req.Status {
			return &protocol.UpdateRtcStatusResponse{
				Result: protocol.UpdateRtcStatusResult{Success: true},
			}, nil
		}
		return nil, &APIError{
			Code:    "rtc.invalid_state",
			Message: fmt.Sprintf("rtc %s is %s, cannot update to %s", req.RtcId, rtc.Status, req.Status),
		}
	}

	// 归属校验：通过 RTC 的 sessionID 校验用户权限
	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, rtc.SessionID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}

	// Load session BEFORE the transaction for event publishing.
	// Event publishing is best-effort — the OwnerRefID (used for routing)
	// never changes after session creation, so pre-transaction data is
	// sufficient. If the pre-load fails, we proceed with nil session and
	// the update publisher skips event delivery; the frontend will reload
	// the latest state when it notices the RTC status change.
	sessionBefore, sessionLoadErr := h.deps.SessionRepo.GetByID(ctx, rtc.SessionID)
	if sessionLoadErr != nil {
		logger.Warn(ctx, "[UpdateRtcStatus] pre-load session failed (will skip event publishing)",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(sessionLoadErr))
	}

	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.UpdateRtcStatus(txCtx, h.deps.Deps, rtcUUID, req.Status); err != nil {
			return nil, err
		}
		// Use pre-loaded session for routing metadata. Do not reload inside
		// the transaction — a transient DB error on reload must not abort
		// the RTC status update.
		return primitives.BuildRtcStatusUpdates(sessionBefore, rtcUUID), nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[UpdateRtcStatus] push failed after commit (data safe)", zap.Error(err))
		} else {
			return nil, h.internalError(ctx, "rtc.error", "internal error", err)
		}
	}

	return &protocol.UpdateRtcStatusResponse{
		Result:  protocol.UpdateRtcStatusResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}
