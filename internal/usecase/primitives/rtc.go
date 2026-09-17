// internal/usecase/primitives/rtc.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// ResolveRtcSessionID resolves a session ID from an RTC ID.
func ResolveRtcSessionID(
	ctx context.Context,
	deps *usecase.Dependencies,
	rtcID uuid.UUID,
) (uuid.UUID, error) {
	rtc, err := deps.RtcRepo.GetByID(ctx, rtcID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get rtc: %w", err)
	}
	return rtc.SessionID, nil
}

// UpdateRtcStatus updates RTC status inside a transaction (thin wrapper).
func UpdateRtcStatus(
	txCtx context.Context,
	deps *usecase.Dependencies,
	rtcID uuid.UUID,
	status protocol.RtcStatus,
) error {
	if err := deps.RtcRepo.UpdateStatus(txCtx, rtcID, status); err != nil {
		return fmt.Errorf("update rtc status: %w", err)
	}
	return nil
}

// UpdateRtcResult updates the RTC result (status + result + error + completed_at)
// inside a transaction.
func UpdateRtcResult(
	txCtx context.Context,
	deps *usecase.Dependencies,
	rtcID uuid.UUID,
	status protocol.RtcStatus,
	result *string,
	errMsg string,
) error {
	if err := deps.RtcRepo.UpdateResult(txCtx, rtcID, status, result, errMsg); err != nil {
		return fmt.Errorf("update rtc result: %w", err)
	}
	return nil
}
