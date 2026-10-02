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
	if additionalBytes < 0 {
		return "", fmt.Errorf("additionalBytes must be non-negative, got %d", additionalBytes)
	}
	// Zero-byte uploads (e.g., empty files) are allowed but skip quota reservation.
	// They consume no storage, so there is nothing to reserve or commit.
	if additionalBytes == 0 {
		return "", nil
	}
	requestID = uuid.New().String()
	quotaKey := cache.OSS3Quota(userID)
	pendingKey := cache.OSS3QuotaPending(userID, requestID)
	aggKey := cache.OSS3QuotaPendingAgg(userID)
	// Pending key prefix for constructing other requests' pending keys during ZSET scan.
	// Format: "oss3:quota:pending:{uid}:" — append requestID to get a specific pending key.
	pendingKeyPrefix := cache.PrefixOSS3QuotaPending + userID + ":"

	script := uc.scripts[cache.OSS3ScriptQuotaReserve]
	// Lua script expects TTL in seconds (e.g., 300 for 5 minutes)
	pendingTTLSeconds := int(uc.cfg.Quota.PendingTTL.Seconds())
	result, err := script.Run(ctx, uc.redis,
		[]string{quotaKey, pendingKey, aggKey},
		additionalBytes,
		uc.cfg.Quota.MaxUserQuotaBytes,
		pendingTTLSeconds,
		requestID,
		pendingKeyPrefix,
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
	aggKey := cache.OSS3QuotaPendingAgg(userID)

	script := uc.scripts[cache.OSS3ScriptQuotaCommit]
	result, err := script.Run(ctx, uc.redis,
		[]string{quotaKey, pendingKey, aggKey},
		amount,
		requestID,
	).Int()
	if err != nil {
		// Quota commit Lua script failed — clean up the marker so a retry can succeed.
		// If cleanup itself fails, log at Error level so operators can investigate;
		// the marker will expire via TTL (24h) but will block retries until then.
		if delErr := uc.redis.Del(ctx, markerKey).Err(); delErr != nil {
			logger.Error(ctx, "failed to cleanup quota commit marker after Lua failure",
				zap.String("user_id", userID),
				zap.String("request_id", requestID),
				zap.Error(delErr))
		}
		return fmt.Errorf("quota commit: %w", err)
	}
	// Handle Lua script return values:
	//   1: success — pending committed to quota counter
	//   0: pending key missing (expired or already committed)
	//  -1: amount exceeds pending (caller bug)
	switch result {
	case 1:
		return nil
	case 0:
		// Pending expired before commit — the reservation was lost.
		// Log at Warn level so operators can track frequency; the upload succeeded
		// but quota accounting is now inconsistent (data written without quota recorded).
		// Clean up the marker since there is nothing to retry.
		if delErr := uc.redis.Del(ctx, markerKey).Err(); delErr != nil {
			logger.Error(ctx, "failed to cleanup quota commit marker after pending expiry",
				zap.String("user_id", userID),
				zap.String("request_id", requestID),
				zap.Error(delErr))
		}
		logger.Warn(ctx, "quota commit skipped: pending reservation expired",
			zap.String("user_id", userID),
			zap.String("request_id", requestID),
			zap.Int64("amount", amount))
		return fmt.Errorf("quota commit: pending expired for request %s: %w", requestID, rtcoss3.ErrQuotaExceeded)
	case -1:
		// Amount exceeds reserved pending — indicates a caller bug (e.g., wrong amount passed).
		// Log at Error level as this should never happen in correct code paths.
		// Clean up the marker since the commit is invalid.
		if delErr := uc.redis.Del(ctx, markerKey).Err(); delErr != nil {
			logger.Error(ctx, "failed to cleanup quota commit marker after amount overflow",
				zap.String("user_id", userID),
				zap.String("request_id", requestID),
				zap.Error(delErr))
		}
		logger.Error(ctx, "quota commit failed: amount exceeds pending reservation (caller bug)",
			zap.String("user_id", userID),
			zap.String("request_id", requestID),
			zap.Int64("amount", amount))
		return fmt.Errorf("quota commit: amount %d exceeds pending for request %s", amount, requestID)
	default:
		// Unexpected return value — defensive programming.
		logger.Error(ctx, "quota commit returned unexpected value",
			zap.String("user_id", userID),
			zap.String("request_id", requestID),
			zap.Int("result", result))
		return fmt.Errorf("quota commit: unexpected return value %d", result)
	}
}

// ReleaseQuota cancels a pending reservation (on upload failure or abort).
//
// Flow: Lua script OSS3QuotaRollback deletes the pending key.
// If the pending key already expired (TTL), this is a no-op.
func (uc *OSS3Usecase) ReleaseQuota(ctx context.Context, userID, requestID string) {
	pendingKey := cache.OSS3QuotaPending(userID, requestID)
	aggKey := cache.OSS3QuotaPendingAgg(userID)

	script := uc.scripts[cache.OSS3ScriptQuotaRollback]
	result, err := script.Run(ctx, uc.redis, []string{pendingKey, aggKey}, requestID).Int()
	if err != nil {
		// Log at Warn level so persistent Redis issues are visible in monitoring.
		// The pending key will eventually expire via TTL, but operators should
		// be aware of ongoing failures.
		logger.Warn(ctx, "quota release failed (best-effort, TTL will cleanup)",
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

// ReleaseQuotaForDeletedFile releases quota after a file is deleted.
// Encapsulates the compensation logic so handlers only need to orchestrate.
// Uses Background context internally to avoid cancellation issues when the client disconnects.
// Logs errors but does not fail the caller — reconciliation will sync eventually.
func (uc *OSS3Usecase) ReleaseQuotaForDeletedFile(ctx context.Context, userID, key string, fileSize int64) {
	if fileSize <= 0 {
		return
	}
	// Use Background context to avoid cancellation when the client disconnects.
	bgCtx := context.Background()
	if err := uc.AdjustQuota(bgCtx, userID, -fileSize); err != nil {
		logger.Warn(bgCtx, "failed to adjust quota after delete",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Int64("size", fileSize),
			zap.Error(err))
	}
}

// ReleaseMultipartUploadQuota releases quota for discarded multipart upload parts.
// Used by the instant upload path when an existing file is detected and parts are discarded.
// Calculates total part size and releases quota atomically.
// Uses Background context internally to avoid cancellation issues.
// Logs errors but does not fail the caller — reconciliation will sync eventually.
func (uc *OSS3Usecase) ReleaseMultipartUploadQuota(ctx context.Context, userID, uploadID string) {
	bgCtx := context.Background()
	partsTotalSize, err := uc.GetMultipartUploadTotalSize(bgCtx, uploadID)
	if err != nil {
		logger.Warn(ctx, "failed to get parts total size for quota release",
			zap.String("user_id", userID),
			zap.String("upload_id", uploadID),
			zap.Error(err))
		return
	}
	if partsTotalSize <= 0 {
		return
	}
	if err := uc.AdjustQuota(bgCtx, userID, -partsTotalSize); err != nil {
		logger.Warn(ctx, "failed to release quota for multipart upload",
			zap.String("user_id", userID),
			zap.String("upload_id", uploadID),
			zap.Int64("parts_total_size", partsTotalSize),
			zap.Error(err))
	}
}
