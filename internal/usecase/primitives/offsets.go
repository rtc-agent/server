// internal/usecase/primitives/offsets.go
package primitives

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/usecase"

	"github.com/google/uuid"
)

// AllocateOffsets 一次 Redis 调用同时为 session 和 turn 分配 offset。
// turnID 为 nil 时只分配 globalOffset（消息不属于任何 Turn）。
// count 指定批量分配数量（>=1），返回起始 offset。
func AllocateOffsets(
	ctx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	turnID *uuid.UUID,
	count int,
) (startGlobalOffset uint32, startTurnOffset *uint32, err error) {
	if count < 1 {
		count = 1
	}
	keys := []string{cache.SessionMsgOffset(sessionID.String())}
	argv := []any{count}
	if turnID != nil {
		keys = append(keys, cache.TurnMsgOffset(turnID.String()))
		argv = append(argv, count)
	}
	result, err := cache.BatchIncrOffset.Run(ctx, deps.Redis, keys, argv...).Result()
	if err != nil {
		return 0, nil, fmt.Errorf("incr offsets: %w", err)
	}
	arr, ok := result.([]any)
	if !ok || len(arr) != len(keys) {
		return 0, nil, fmt.Errorf("invalid batch incr result: %#v", result)
	}
	globalOff, ok := arr[0].(int64)
	if !ok {
		return 0, nil, fmt.Errorf("invalid global offset type: %T", arr[0])
	}
	// 返回起始 offset = 结束 offset - count + 1
	startGlobal := uint32(globalOff) - uint32(count) + 1
	if turnID == nil {
		return startGlobal, nil, nil
	}
	turnOff, ok := arr[1].(int64)
	if !ok {
		return 0, nil, fmt.Errorf("invalid turn offset type: %T", arr[1])
	}
	startTurn := uint32(turnOff) - uint32(count) + 1
	return startGlobal, &startTurn, nil
}

// AllocateRtcOffset 为 RTC 记录分配独立的 offset。
// 使用单独的 Redis 计数器（session:rtc_offset:{sessionID}），
// 与消息 global_offset 计数器分离，避免 RTC 创建时消耗消息 offset 导致跳空。
func AllocateRtcOffset(
	ctx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
) (uint32, error) {
	keys := []string{cache.SessionRtcOffset(sessionID.String())}
	argv := []any{1}
	result, err := cache.BatchIncrOffset.Run(ctx, deps.Redis, keys, argv...).Result()
	if err != nil {
		return 0, fmt.Errorf("incr rtc offset: %w", err)
	}
	arr, ok := result.([]any)
	if !ok || len(arr) != 1 {
		return 0, fmt.Errorf("invalid rtc offset result: %#v", result)
	}
	off, ok := arr[0].(int64)
	if !ok {
		return 0, fmt.Errorf("invalid rtc offset type: %T", arr[0])
	}
	return uint32(off), nil
}
