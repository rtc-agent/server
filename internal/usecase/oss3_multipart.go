package usecase

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.uber.org/zap"
)

// CompleteMultipartUploadWithComp completes a multipart upload with transaction boundary and compensation.
// If DB operations fail after backend success, it attempts to clean up the backend object.
//
// Flow:
// 1. Complete multipart upload in backend (MinIO)
// 2. HeadObject to get actual size (may differ from parts sum due to compression/dedup)
// 3. Create file record in DB
// 4. Delete multipart upload record from DB
// 5. If step 3 or 4 fails, attempt to delete the backend object (compensation)
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
		// Compensation: try to delete the backend object
		if compErr := uc.backend.DeleteObject(ctx, bucket, key); compErr != nil {
			logger.Warn(ctx, "CompleteMultipartUpload HeadObject failed, compensation also failed",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Error(compErr))
			// Include both errors for full context
			return "", fmt.Errorf("head object after complete: %w; compensation also failed: %v", err, compErr)
		}
		return "", fmt.Errorf("head object after complete, backend object cleaned up: %w", err)
	}

	// Step 3 & 4: DB operations
	dbErr := uc.completeMultipartUploadDBOps(ctx, userID, bucket, key, uploadID, etag, meta.Size)
	if dbErr != nil {
		// Compensation: try to delete the backend object
		if compErr := uc.backend.DeleteObject(ctx, bucket, key); compErr != nil {
			// Both DB and compensation failed — log and return combined error
			logger.Warn(ctx, "CompleteMultipartUpload compensation failed",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Error(compErr))
			return "", fmt.Errorf("DB operations failed: %w; compensation also failed: %v", dbErr, compErr)
		}
		return "", fmt.Errorf("DB operations failed, backend object cleaned up: %w", dbErr)
	}

	return etag, nil
}

// completeMultipartUploadDBOps performs DB operations for completing a multipart upload.
// Note: These operations are not wrapped in a transaction due to repository layer limitations.
// If step 2 or 3 fails after step 1 succeeds, compensation will clean up the backend object.
func (uc *OSS3Usecase) completeMultipartUploadDBOps(
	ctx context.Context,
	userID, bucket, key, uploadID, etag string,
	size int64,
) error {
	// Create file record with actual size from backend (not calculated from parts)
	file := &model.File{
		UserID: userID,
		Bucket: bucket,
		Key:    key,
		Size:   size,
		ETag:   etag,
	}
	if err := uc.fileRepo.Create(ctx, file); err != nil {
		return fmt.Errorf("create file record: %w", err)
	}

	// Delete multipart upload record (this should also delete parts via cascade)
	if err := uc.DeleteMultipartUploadRecord(ctx, uploadID); err != nil {
		return fmt.Errorf("delete upload record: %w", err)
	}

	return nil
}

// PutObjectWithComp uploads an object with transaction boundary and compensation.
// If DB operations fail after backend success, it attempts to clean up the backend object.
//
// Flow:
// 1. Put object in backend
// 2. Commit quota
// 3. Create file record in DB
// 4. If step 3 fails, attempt to delete the backend object (compensation)
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
		// Compensation: try to delete the backend object
		if compErr := uc.backend.DeleteObject(ctx, bucket, key); compErr != nil {
			// Both DB and compensation failed
			logger.Warn(ctx, "PutObject compensation failed",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Error(compErr))
			return fmt.Errorf("create file record failed: %w; compensation also failed: %v", err, compErr)
		}
		return fmt.Errorf("create file record failed, backend object cleaned up: %w", err)
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
