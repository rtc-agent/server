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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// UpdateRtcStatus updates RTC execution status (executing/failed/timeout/rejected).
func (h *Handler) UpdateRtcStatus(ctx context.Context, req *protocol.UpdateRtcStatusRequest) (*protocol.UpdateRtcStatusResponse, error) {
	ctx, span := otel.GetTracerProvider().Tracer("rpc").Start(ctx, "rpc.updateRtcStatus",
		trace.WithAttributes(
			attribute.String("rtc.id", req.RtcId),
			attribute.String("rtc.status", string(req.Status)),
		),
	)
	defer span.End()

	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		span.SetStatus(codes.Error, "missing user_id in context")
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	rtcUUID, apiErr := parseUUID(req.RtcId, "rtc_id")
	if apiErr != nil {
		span.SetStatus(codes.Error, apiErr.Message)
		return nil, apiErr
	}

	span.SetAttributes(attribute.String("user.id", userID.String()))

	logger.Info(ctx, "[UpdateRtcStatus]",
		zap.String("user", userID.String()),
		zap.String("rtc", req.RtcId),
		zap.String("status", string(req.Status)))

	// Validate target status.
	switch req.Status {
	case protocol.RtcStatusExecuting, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		// valid
	default:
		span.SetStatus(codes.Error, "rtc.invalid_status")
		return nil, &APIError{
			Code:    "rtc.invalid_status",
			Message: fmt.Sprintf("invalid target status: %s", req.Status),
		}
	}

	// Load RTC and verify existence.
	rtc, err := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			span.SetStatus(codes.Error, "rtc.not_found")
			return nil, &APIError{Code: "rtc.not_found", Message: fmt.Sprintf("rtc %s not found", req.RtcId)}
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return nil, h.internalError(ctx, "rtc.error", "internal error", err)
	}

	// State validation: terminal states cannot be changed (idempotent: if target matches current, return success).
	switch protocol.RtcStatus(rtc.Status) {
	case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		if protocol.RtcStatus(rtc.Status) == req.Status {
			return &protocol.UpdateRtcStatusResponse{
				Result: protocol.UpdateRtcStatusResult{Success: true},
			}, nil
		}
		span.SetStatus(codes.Error, "rtc.invalid_state")
		return nil, &APIError{
			Code:    "rtc.invalid_state",
			Message: fmt.Sprintf("rtc %s is %s, cannot update to %s", req.RtcId, rtc.Status, req.Status),
		}
	}

	// Ownership check: verify user permissions via RTC's sessionID.
	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, rtc.SessionID, creator); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, h.ownershipError(ctx, err)
	}

	span.SetAttributes(attribute.String("session.id", rtc.SessionID.String()))

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
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			return nil, h.internalError(ctx, "rtc.error", "internal error", err)
		}
	}

	// For terminal statuses, also trigger batch completion logic.
	// This handles cases where RTC times out or is rejected without a result submission.
	switch req.Status {
	case protocol.RtcStatusFailed, protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		// Reload RTC to get latest state for batch processing
		updatedRtc, reloadErr := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
		if reloadErr == nil && updatedRtc != nil {
			h.resumeTurnAfterRtc(ctx, updatedRtc)
		}
	}

	return &protocol.UpdateRtcStatusResponse{
		Result:  protocol.UpdateRtcStatusResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}
