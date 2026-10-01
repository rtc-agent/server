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
	if additionalBytes <= 0 {
		return "", fmt.Errorf("additionalBytes must be positive, got %d", additionalBytes)
	}
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
		return "", fmt.Errorf("user %s: %w", userID, rtcoss3.ErrQuotaExceeded)
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

// AdjustQuota adjusts the committed quota counter by a delta.
// Positive delta adds to usage; negative delta subtracts.
// Used to release quota when instant upload detects duplicate content in multipart uploads.
//
// Flow: Lua script OSS3QuotaAdjust atomically adds delta to quota counter.
// Returns error if the adjustment would make quota negative.
func (uc *OSS3Usecase) AdjustQuota(ctx context.Context, userID string, deltaBytes int64) error {
	quotaKey := cache.OSS3Quota(userID)

	script := uc.scripts[cache.OSS3ScriptQuotaAdjust]
	result, err := script.Run(ctx, uc.redis,
		[]string{quotaKey},
		deltaBytes,
	).Int()
	if err != nil {
		return fmt.Errorf("quota adjust: %w", err)
	}
	if result == 0 {
		return fmt.Errorf("quota adjust would go negative for user %s", userID)
	}
	return nil
}

// GetMultipartUploadTotalSize calculates the total size of all parts for a multipart upload.
// Used by instant upload path to release quota committed during UploadPart.
func (uc *OSS3Usecase) GetMultipartUploadTotalSize(ctx context.Context, uploadID string) (int64, error) {
	upload, err := uc.uploadRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return 0, fmt.Errorf("get upload record: %w", err)
	}

	parts, err := uc.uploadRepo.ListParts(ctx, upload.ID)
	if err != nil {
		return 0, fmt.Errorf("list parts: %w", err)
	}

	var totalSize int64
	for _, p := range parts {
		totalSize += p.Size
	}
	return totalSize, nil
}
