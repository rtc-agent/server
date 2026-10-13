// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rtc-agent/server/internal/infra/cache"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// RtcBatchResumeUsecase handles batch RTC resume operations.
// It encapsulates Redis operations for batch resume functionality.
type RtcBatchResumeUsecase struct {
	redis redis.UniversalClient
}

// NewRtcBatchResumeUsecase creates a new RtcBatchResumeUsecase.
func NewRtcBatchResumeUsecase(redis redis.UniversalClient) *RtcBatchResumeUsecase {
	return &RtcBatchResumeUsecase{
		redis: redis,
	}
}

// SetOrphanTrigger attempts to set an orphan trigger key in Redis.
// Returns true if the key was set (i.e., this is the first attempt),
// false if the key already exists (i.e., already triggered).
func (uc *RtcBatchResumeUsecase) SetOrphanTrigger(ctx context.Context, rtcID string, ttl time.Duration) (bool, error) {
	if uc.redis == nil {
		return false, nil
	}

	orphanKey := cache.RtcOrphanTriggered(rtcID)
	ok, err := uc.redis.SetNX(ctx, orphanKey, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("setnx orphan key failed: %w", err)
	}
	return ok, nil
}

// DeleteOrphanTrigger deletes the orphan trigger key from Redis.
func (uc *RtcBatchResumeUsecase) DeleteOrphanTrigger(ctx context.Context, rtcID string) error {
	if uc.redis == nil {
		return nil
	}

	orphanKey := cache.RtcOrphanTriggered(rtcID)
	if err := uc.redis.Del(ctx, orphanKey).Err(); err != nil {
		return fmt.Errorf("del orphan key failed: %w", err)
	}
	return nil
}

// BatchResumeResult holds the result of a batch resume attempt.
type BatchResumeResult struct {
	// Items contains the batch resume items if batch is complete.
	// Nil if batch is still in progress or doesn't exist.
	Items []turnagent.BatchResumeItem
	// InProgress indicates whether the batch is still in progress.
	// If true, the caller should return and wait for more RTCs.
	// If false, the caller should continue with normal resume logic.
	InProgress bool
	// Exists indicates whether the batch key exists.
	// If false, the batch doesn't exist (TTL expired or never created).
	Exists bool
}

// TryCollectBatchResume attempts to collect batch resume items via Redis.
// Returns (items, true) if the batch is still in progress (caller should return),
// or (items, false) if the caller should continue with normal resume logic.
func (uc *RtcBatchResumeUsecase) TryCollectBatchResume(ctx context.Context, rtcID, turnID, result, status, errorMessage string) (*BatchResumeResult, error) {
	if uc.redis == nil {
		return &BatchResumeResult{Exists: false}, nil
	}

	batchKey := cache.RtcBatchPending(turnID)
	resultsKey := cache.RtcBatchResults(turnID)

	// Use the provided result or error message
	resultStr := result
	if status == "failed" && errorMessage != "" {
		resultStr = errorMessage
	}

	remaining, scriptErr := cache.BatchComplete.Run(
		ctx, uc.redis,
		[]string{batchKey, resultsKey},
		rtcID, resultStr, 600,
	).Int64()

	if scriptErr != nil {
		return nil, fmt.Errorf("batch complete script failed: %w", scriptErr)
	}

	if remaining == -1 {
		// Batch key doesn't exist (TTL expired or never created)
		return &BatchResumeResult{Exists: false}, nil
	}

	if remaining > 0 {
		// More RTCs to go
		return &BatchResumeResult{InProgress: true, Exists: true}, nil
	}

	// All RTCs completed — build BatchResumeItems from stored data
	interruptMapKey := cache.RtcBatchInterruptMap(turnID)
	items, err := uc.buildBatchResumeItems(ctx, resultsKey, interruptMapKey, batchKey)
	if err != nil {
		return nil, err
	}

	// If interrupt map was empty (single interrupt scenario), return nil to let
	// the caller fall back to single resume path using InterruptID/InterruptResult.
	// This handles the case where registerRtcBatch was called but interruptTurn
	// didn't write to interrupt map because len(allInterruptContexts) == 1.
	if len(items) == 0 {
		return &BatchResumeResult{Exists: false}, nil
	}

	return &BatchResumeResult{Items: items, Exists: true}, nil
}

// buildBatchResumeItems reads batch results and interrupt map from Redis,
// then cleans up all batch-related keys.
func (uc *RtcBatchResumeUsecase) buildBatchResumeItems(ctx context.Context, resultsKey, interruptMapKey, batchKey string) ([]turnagent.BatchResumeItem, error) {
	results, resultsErr := uc.redis.HGetAll(ctx, resultsKey).Result()
	if resultsErr != nil {
		return nil, fmt.Errorf("batch results read failed: %w", resultsErr)
	}

	interruptMap, mapErr := uc.redis.HGetAll(ctx, interruptMapKey).Result()
	if mapErr != nil {
		return nil, fmt.Errorf("batch interrupt map read failed: %w", mapErr)
	}

	items := make([]turnagent.BatchResumeItem, 0, len(interruptMap))
	for rtcID, interruptID := range interruptMap {
		result := results[rtcID]
		items = append(items, turnagent.BatchResumeItem{
			InterruptID: interruptID,
			Result:      result,
		})
	}

	// Clean up batch keys
	if err := uc.redis.Del(ctx, batchKey, resultsKey, interruptMapKey).Err(); err != nil {
		return nil, fmt.Errorf("batch cleanup failed: %w", err)
	}

	return items, nil
}
