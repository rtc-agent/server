package usecase

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/cache"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// CheckAndReserveQuota atomically reserves quota for an upload.
// Returns a requestID that must be passed to CommitQuota or ReleaseQuota.
//
// Flow: Lua script OSS3QuotaReserve checks (current + pending + amount <= maxQuota),
// creates a pending key with TTL, all in one atomic call.
func (uc *OSS3Usecase) CheckAndReserveQuota(ctx context.Context, userID string, additionalBytes int64) (requestID string, err error) {
	requestID = uuid.New().String()
	quotaKey := cache.OSS3Quota(userID)
	pendingKey := cache.OSS3QuotaPending(userID, requestID)

	script := uc.scripts[cache.OSS3ScriptQuotaReserve]
	result, err := script.Run(ctx, uc.redis,
		[]string{quotaKey, pendingKey},
		additionalBytes,
		uc.cfg.Quota.MaxUserQuotaBytes,
		int(uc.cfg.Quota.PendingTTL.Seconds()), // e.g. 300
	).Int()
	if err != nil {
		return "", fmt.Errorf("quota reserve: %w", err)
	}
	if result == 0 {
		return "", rtcoss3.ErrRequestQuotaExceeded
	}
	return requestID, nil
}

// CommitQuota finalizes a previously reserved quota.
// Called after PutObject/UploadPart succeeds (data actually written to storage).
//
// Flow: Lua script OSS3QuotaCommit atomically adds pending amount to quota and deletes pending key.
func (uc *OSS3Usecase) CommitQuota(ctx context.Context, userID, requestID string, amount int64) error {
	quotaKey := cache.OSS3Quota(userID)
	pendingKey := cache.OSS3QuotaPending(userID, requestID)

	script := uc.scripts[cache.OSS3ScriptQuotaCommit]
	_, err := script.Run(ctx, uc.redis,
		[]string{quotaKey, pendingKey},
		amount,
	).Int()
	if err != nil {
		return fmt.Errorf("quota commit: %w", err)
	}
	return nil
}

// ReleaseQuota cancels a pending reservation (on upload failure or abort).
//
// Flow: Lua script OSS3QuotaRollback deletes the pending key.
// If the pending key already expired (TTL), this is a no-op.
func (uc *OSS3Usecase) ReleaseQuota(ctx context.Context, userID, requestID string) {
	pendingKey := cache.OSS3QuotaPending(userID, requestID)

	script := uc.scripts[cache.OSS3ScriptQuotaRollback]
	_, _ = script.Run(ctx, uc.redis, []string{pendingKey}).Int()
	// best-effort; TTL guarantees eventual cleanup even on crash
}
