package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// AcquireUploadPartLock acquires a distributed lock for an upload part operation.
// The lock is keyed by uploadID, ensuring that concurrent UploadPart requests for the
// same upload are serialized, preventing TOCTOU races between ListParts and UploadPart.
//
// Returns (holderUUID, true) on success, ("", false) if another request holds the lock.
// The caller MUST call ReleaseUploadPartLock with the returned holderUUID when done.
func (uc *OSS3Usecase) AcquireUploadPartLock(ctx context.Context, uploadID string, ttl time.Duration) (string, bool, error) {
	holderUUID := uuid.New().String()
	lockKey := cache.OSS3Lock("upload:" + uploadID)

	script := uc.scripts[cache.OSS3ScriptLockAcquire]
	result, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
		int(ttl.Seconds()),
	).Int()
	if err != nil {
		return "", false, fmt.Errorf("acquire upload part lock: %w", err)
	}
	if result == 0 {
		return "", false, nil
	}
	return holderUUID, true, nil
}

// ReleaseUploadPartLock releases the distributed lock for an upload part operation.
// Only the holder that acquired the lock can release it (verified by holderUUID).
func (uc *OSS3Usecase) ReleaseUploadPartLock(ctx context.Context, uploadID string, holderUUID string) {
	lockKey := cache.OSS3Lock("upload:" + uploadID)

	script := uc.scripts[cache.OSS3ScriptLockRelease]
	_, err := script.Run(ctx, uc.redis,
		[]string{lockKey},
		holderUUID,
	).Int()
	if err != nil {
		// Best-effort release; lock will expire via TTL if release fails.
		logger.Debug(ctx, "release upload part lock failed (best-effort)",
			zap.String("upload_id", uploadID),
			zap.String("holder", holderUUID),
			zap.Error(err))
	}
}

// CompleteMultipartUploadWithComp completes a multipart upload with transaction boundary and compensation.
// If DB operations fail after backend success, it attempts to clean up the backend object asynchronously.
//
// Flow:
// 1. Complete multipart upload in backend (MinIO)
// 2. HeadObject to get actual size (may differ from parts sum due to compression/dedup)
// 3. Create file record in DB
// 4. Delete multipart upload record from DB
// 5. If step 3 or 4 fails, attempt async deletion of the backend object (compensation)
//
// Compensation is async (best-effort) to avoid blocking the error response.
// The orphan reconciler provides a second safety net if async compensation also fails.
func (uc *OSS3Usecase) CompleteMultipartUploadWithComp(
	ctx context.Context,
	userID, bucket, key, uploadID string,
	parts []rtcoss3.CompletedPart,
) (etag string, err error) {
	// Step 1: Complete in backend
	etag, err = uc.backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts)
	if err != nil {
		return "", fmt.Errorf("complete multipart upload in backend: %w", err)
	}

	// Step 2: Get actual object size from backend
	// The size may differ from the sum of parts due to backend compression or deduplication
	meta, err := uc.backend.HeadObject(ctx, bucket, key)
	if err != nil {
		// Compensation: async delete the backend object (don't block on this)
		uc.asyncCleanupBackendObject(bucket, key, "HeadObject after CompleteMultipartUpload")
		return "", fmt.Errorf("head object after complete: %w", err)
	}

	// Step 3 & 4: DB operations
	dbErr := uc.completeMultipartUploadDBOps(ctx, userID, bucket, key, uploadID, etag, meta.Size)
	if dbErr != nil {
		// Compensation: async delete the backend object (don't block on this)
		uc.asyncCleanupBackendObject(bucket, key, "DB operations after CompleteMultipartUpload")
		return "", fmt.Errorf("DB operations failed: %w", dbErr)
	}

	return etag, nil
}

// asyncCleanupBackendObject schedules an asynchronous deletion of a backend object.
// Used for compensation when DB operations fail after backend success.
// A separate context with timeout is used because the original request context may
// be cancelled when the response is sent.
func (uc *OSS3Usecase) asyncCleanupBackendObject(bucket, key, reason string) {
	// Create a detached context with timeout for the cleanup operation.
	// We use context.Background() because the original ctx may be cancelled
	// as soon as we return the error response.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger.SafeGo("async-backend-cleanup", func() {
		if compErr := uc.backend.DeleteObject(cleanupCtx, bucket, key); compErr != nil {
			logger.Warn(cleanupCtx, "async backend cleanup failed (orphan reconciler will retry)",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.String("reason", reason),
				zap.Error(compErr))
		} else {
			logger.Info(cleanupCtx, "async backend cleanup succeeded",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.String("reason", reason))
		}
	})
}

// completeMultipartUploadDBOps performs DB operations for completing a multipart upload.
// All operations are wrapped in a single transaction to ensure atomicity:
// if any operation fails, all changes are rolled back to prevent orphaned records.
func (uc *OSS3Usecase) completeMultipartUploadDBOps(
	ctx context.Context,
	userID, bucket, key, uploadID, etag string,
	size int64,
) error {
	return uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// Create file record with actual size from backend (not calculated from parts)
		file := &model.File{
			UserID: userID,
			Bucket: bucket,
			Key:    key,
			Size:   size,
			ETag:   etag,
		}
		if err := uc.fileRepo.Create(txCtx, file); err != nil {
			return fmt.Errorf("create file record: %w", err)
		}

		// Delete multipart upload record and its parts atomically
		if err := uc.deleteUploadInTx(txCtx, tx, uploadID); err != nil {
			return fmt.Errorf("delete upload record: %w", err)
		}

		return nil
	})
}

// PutObjectWithComp uploads an object with transaction boundary and compensation.
// If DB operations fail after backend success, it attempts to clean up the backend object asynchronously.
//
// Flow:
// 1. Put object in backend
// 2. Commit quota
// 3. Create file record in DB
// 4. If step 3 fails, attempt async deletion of the backend object (compensation)
func (uc *OSS3Usecase) PutObjectWithComp(
	ctx context.Context,
	userID, bucket, key string,
	size int64,
	contentType string,
	etag string,
) error {
	// Step 2: Commit quota (already done in handler, but we document it here)
	// Step 3: Create file record in DB
	file := &model.File{
		UserID:      userID,
		Bucket:      bucket,
		Key:         key,
		Size:        size,
		ContentType: contentType,
		ETag:        etag,
	}
	if err := uc.fileRepo.Create(ctx, file); err != nil {
		// Compensation: async delete the backend object (don't block on this)
		uc.asyncCleanupBackendObject(bucket, key, "DB operations after PutObject")
		return fmt.Errorf("create file record failed: %w", err)
	}

	return nil
}

// InstantUploadResult represents the result of an instant upload check.
type InstantUploadResult struct {
	// Hit indicates whether this is an instant upload hit (file already exists)
	Hit bool
	// FileMeta contains the file metadata if hit
	FileMeta *FileRecord
	// ConsistencyViolation indicates DB and MinIO are out of sync
	ConsistencyViolation bool
	// ViolationType describes the type of consistency violation
	ViolationType string // "db_has_minio_missing" or "minio_has_db_missing"
}

// CheckInstantUpload checks if a file upload can be satisfied by existing data.
// M2: Extracted from handler to reduce cognitive complexity.
//
// Returns 4 possible states:
//  1. Both DB and MinIO have the file → instant upload hit
//  2. DB has record but MinIO missing → consistency violation (re-upload needed)
//  3. MinIO has file but DB missing → consistency repair (return success, repair DB)
//  4. Neither has file → normal upload flow
func (uc *OSS3Usecase) CheckInstantUpload(ctx context.Context, userID, bucket, key string) (*InstantUploadResult, error) {
	result := &InstantUploadResult{}

	// Check DB first
	existingFile, err := uc.GetFileRecord(ctx, userID, key)
	if err != nil {
		return nil, fmt.Errorf("get file record: %w", err)
	}

	if existingFile != nil {
		// DB record exists, verify MinIO file still exists
		meta, headErr := uc.backend.HeadObject(ctx, bucket, key)
		if headErr == nil && meta.Key != "" {
			// Both DB and MinIO have the file → instant upload hit
			result.Hit = true
			result.FileMeta = existingFile
			return result, nil
		}
		// DB has record but MinIO missing → consistency violation
		result.ConsistencyViolation = true
		result.ViolationType = "db_has_minio_missing"
		return result, nil
	}

	// DB has no record, check if MinIO has the file
	existingMeta, err := uc.backend.HeadObject(ctx, bucket, key)
	if err == nil && existingMeta.Key != "" {
		// MinIO has file but DB missing → consistency repair
		result.Hit = true
		result.ConsistencyViolation = true
		result.ViolationType = "minio_has_db_missing"
		result.FileMeta = &FileRecord{
			UserID:      userID,
			Bucket:      bucket,
			Key:         key,
			Size:        existingMeta.Size,
			ContentType: existingMeta.ContentType,
			ETag:        existingMeta.ETag,
		}
		return result, nil
	}

	// Neither has file → normal upload flow
	return result, nil
}
