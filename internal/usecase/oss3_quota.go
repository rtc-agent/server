package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
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
	// Lua script expects TTL in seconds (e.g., 300 for 5 minutes)
	pendingTTLSeconds := int(uc.cfg.Quota.PendingTTL.Seconds())
	result, err := script.Run(ctx, uc.redis,
		[]string{quotaKey, pendingKey},
		additionalBytes,
		uc.cfg.Quota.MaxUserQuotaBytes,
		pendingTTLSeconds,
	).Int()
	if err != nil {
		return "", fmt.Errorf("quota reserve: %w", err)
	}
	if result == 0 {
		return "", fmt.Errorf("check quota: %w", rtcoss3.ErrQuotaExceeded)
	}
	return requestID, nil
}

// CommitQuota finalizes a previously reserved quota.
// Called after PutObject/UploadPart succeeds (data actually written to storage).
//
// Idempotency: Uses Redis SETNX to create a commit marker before the quota update.
// If the marker already exists (duplicate call from network retry), the commit is skipped.
// The marker has a 24h TTL to cover the retry window.
//
// Flow: SETNX commit marker -> Lua script OSS3QuotaCommit atomically adds pending
// amount to quota and deletes pending key.
func (uc *OSS3Usecase) CommitQuota(ctx context.Context, userID, requestID string, amount int64) error {
	// Idempotency guard: SETNX a commit marker to prevent duplicate commits.
	// If the marker already exists, this commit has already been processed.
	markerKey := cache.OSS3QuotaCommitMarker(userID, requestID)
	ok, err := uc.redis.SetNX(ctx, markerKey, 1, 24*time.Hour).Result()
	if err != nil {
		return fmt.Errorf("quota commit marker: %w", err)
	}
	if !ok {
		// Marker already exists — this commit was already processed (duplicate call).
		return nil
	}

	quotaKey := cache.OSS3Quota(userID)
	pendingKey := cache.OSS3QuotaPending(userID, requestID)

	script := uc.scripts[cache.OSS3ScriptQuotaCommit]
	_, err = script.Run(ctx, uc.redis,
		[]string{quotaKey, pendingKey},
		amount,
	).Int()
	if err != nil {
		// Quota commit Lua script failed — clean up the marker so a retry can succeed.
		uc.redis.Del(ctx, markerKey)
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
	result, err := script.Run(ctx, uc.redis, []string{pendingKey}).Int()
	// LOW-17 fix: add structured logging for best-effort quota release
	if err != nil {
		logger.Debug(ctx, "quota release failed (best-effort, TTL will cleanup)",
			zap.String("user_id", userID),
			zap.String("request_id", requestID),
			zap.Error(err))
	} else {
		logger.Debug(ctx, "quota released",
			zap.String("user_id", userID),
			zap.String("request_id", requestID),
			zap.Int("result", result))
	}
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
		return fmt.Errorf("quota adjust would go negative: %w", rtcoss3.ErrQuotaExceeded)
	}
	return nil
}

// GetMultipartUploadTotalSize calculates the total size of all parts for a multipart upload.
// Used by instant upload path to release quota committed during UploadPart.
// Returns (0, nil) when the upload record does not exist.
func (uc *OSS3Usecase) GetMultipartUploadTotalSize(ctx context.Context, uploadID string) (int64, error) {
	upload, err := uc.uploadRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return 0, fmt.Errorf("get upload record: %w", err)
	}
	if upload == nil {
		return 0, nil
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
