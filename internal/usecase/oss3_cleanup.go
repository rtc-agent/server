package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// CleanupExpiredUploads finds and cleans up expired multipart uploads.
// Returns the number of cleaned uploads.
//
// For each expired upload:
//  1. Abort the upload in the storage backend (best-effort)
//  2. Delete parts from DB
//  3. Delete upload record from DB
//  4. Release any pending quota (best-effort, TTL will handle it)
func (uc *OSS3Usecase) CleanupExpiredUploads(ctx context.Context) (int, error) {
	now := time.Now()
	expired, err := uc.uploadRepo.FindExpired(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("find expired uploads: %w", err)
	}

	cleaned := 0
	for _, upload := range expired {
		// Abort in storage backend (best-effort).
		// Backend may have already cleaned up the upload, so errors are logged
		// at Debug level only — they do not indicate a real problem.
		if uc.backend != nil {
			// LOW-09 fix: use configured bucket instead of hardcoded value
			if abortErr := uc.backend.AbortMultipartUpload(ctx, uc.cfg.MinIO.Bucket, upload.Key, upload.UploadID); abortErr != nil {
				logger.Debug(ctx, "cleanup: backend abort returned error (may already be cleaned up)",
					zap.String("key", upload.Key),
					zap.String("upload_id", upload.UploadID),
					zap.Error(abortErr))
			}
		}

		// Delete parts from DB
		if err := uc.uploadRepo.DeleteParts(ctx, upload.ID); err != nil {
			// Log error but continue
			continue
		}

		// Delete upload record
		if err := uc.uploadRepo.Delete(ctx, upload.ID); err != nil {
			// Log error but continue
			continue
		}

		// Note: Quota release is handled by TTL on pending keys
		// No need to explicitly release here

		cleaned++
	}

	return cleaned, nil
}

// CleanupExpiredCredentials finds and deletes expired credentials.
// Returns the number of deleted credentials.
func (uc *OSS3Usecase) CleanupExpiredCredentials(ctx context.Context) (int, error) {
	now := time.Now()
	deleted, err := uc.credRepo.DeleteExpired(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired credentials: %w", err)
	}
	return int(deleted), nil
}

// RunCleanup performs all cleanup tasks.
// Should be called periodically (e.g., every 5 minutes).
func (uc *OSS3Usecase) RunCleanup(ctx context.Context) error {
	// Cleanup expired uploads
	uploadCount, err := uc.CleanupExpiredUploads(ctx)
	if err != nil {
		return fmt.Errorf("cleanup uploads: %w", err)
	}
	if uploadCount > 0 {
		logger.Info(ctx, "cleanup: expired uploads removed",
			zap.Int("count", uploadCount))
	}

	// Cleanup expired credentials
	credCount, err := uc.CleanupExpiredCredentials(ctx)
	if err != nil {
		return fmt.Errorf("cleanup credentials: %w", err)
	}
	if credCount > 0 {
		logger.Info(ctx, "cleanup: expired credentials removed",
			zap.Int("count", credCount))
	}

	// Reconcile quota: compare DB file sums with Redis counters and fix drift.
	// This is Layer 2 defense — it compensates for commit failures that survive
	// Layer 1 retries. Runs under a distributed lock (single node at a time).
	reconcileResult, reconcileErr := uc.ReconcileQuota(ctx)
	if reconcileErr != nil {
		// Log but do not fail the entire cleanup cycle — reconciliation is
		// best-effort and will retry on the next cycle.
		logger.Error(ctx, "cleanup: quota reconciliation failed",
			zap.Error(reconcileErr))
	} else if reconcileResult != nil && reconcileResult.UsersAdjusted > 0 {
		logger.Info(ctx, "cleanup: quota reconciliation adjusted drift",
			zap.Int("users_checked", reconcileResult.UsersChecked),
			zap.Int("users_adjusted", reconcileResult.UsersAdjusted),
			zap.Int64("total_drift_bytes", reconcileResult.TotalDriftBytes))
	}

	// Reconcile orphans: delete MinIO objects with no DB record.
	// This is the safety net for DB failures that survive immediate compensation.
	orphanResult, orphanErr := uc.ReconcileOrphans(ctx)
	if orphanErr != nil {
		logger.Error(ctx, "cleanup: orphan reconciliation failed",
			zap.Error(orphanErr))
	} else if orphanResult != nil && orphanResult.OrphansFound > 0 {
		logger.Info(ctx, "cleanup: orphan reconciliation found orphans",
			zap.Int("objects_scanned", orphanResult.ObjectsScanned),
			zap.Int("orphans_found", orphanResult.OrphansFound),
			zap.Int("orphans_deleted", orphanResult.OrphansDeleted),
			zap.Int64("bytes_freed", orphanResult.BytesFreed))
	}

	return nil
}

// GetQuotaUsage returns the current quota usage for a user.
// Used by admin API and monitoring.
func (uc *OSS3Usecase) GetQuotaUsage(ctx context.Context, userID string) (int64, error) {
	return uc.fileRepo.SumSizeByUser(ctx, userID)
}

// GetActiveUploadCount returns the number of active uploads for a user.
func (uc *OSS3Usecase) GetActiveUploadCount(ctx context.Context, userID string) (int64, error) {
	return uc.uploadRepo.CountActiveByUser(ctx, userID)
}

// CheckConcurrentUploadLimit returns an error if the user has reached the maximum
// number of concurrent multipart uploads.
func (uc *OSS3Usecase) CheckConcurrentUploadLimit(ctx context.Context, userID string) error {
	maxUploads := uc.cfg.Quota.MaxConcurrentUploads
	if maxUploads <= 0 {
		return nil // no limit configured
	}

	count, err := uc.uploadRepo.CountActiveByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("count active uploads: %w", err)
	}
	if count >= int64(maxUploads) {
		return fmt.Errorf("check concurrent uploads: %w", rtcoss3.ErrMaxUploadsExceeded)
	}
	return nil
}

// GetCredentialByAccessKeyID retrieves a credential by AccessKeyID.
// This is a convenience wrapper around the repo.
func (uc *OSS3Usecase) GetCredentialByAccessKeyID(ctx context.Context, accessKeyID string) (*model.TemporaryCredential, error) {
	return uc.credRepo.GetByAccessKeyID(ctx, accessKeyID)
}
