// internal/usecase/primitives/rtc.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// ResolveRtcSessionID 通过 RTC ID 反查 session ID。
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

// UpdateRtcStatus 在事务内更新 RTC 状态（thin wrapper）。
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

// UpdateRtcResult 在事务内更新 RTC 结果（status + result + error + completed_at）。
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
