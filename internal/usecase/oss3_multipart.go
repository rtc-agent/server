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
// 2. Create file record in DB
// 3. Delete multipart upload record from DB
// 4. If step 2 or 3 fails, attempt to delete the backend object (compensation)
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

	// Step 2 & 3: DB operations
	dbErr := uc.completeMultipartUploadDBOps(ctx, userID, bucket, key, uploadID, etag)
	if dbErr != nil {
		// Compensation: try to delete the backend object
		if compErr := uc.backend.DeleteObject(ctx, bucket, key); compErr != nil {
			// Both DB and compensation failed — log and return DB error
			logger.Warn(ctx, "CompleteMultipartUpload compensation failed",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Error(compErr))
			return "", fmt.Errorf("DB operations failed (compensation also failed): %w", dbErr)
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
) error {
	// Get upload record to determine total size
	upload, err := uc.uploadRepo.GetByUploadID(ctx, uploadID)
	if err != nil {
		return fmt.Errorf("get upload record: %w", err)
	}

	// List parts to calculate total size
	dbParts, err := uc.uploadRepo.ListParts(ctx, upload.ID)
	if err != nil {
		return fmt.Errorf("list parts: %w", err)
	}

	var totalSize int64
	for _, p := range dbParts {
		totalSize += p.Size
	}

	// Create file record
	file := &model.File{
		UserID: userID,
		Bucket: bucket,
		Key:    key,
		Size:   totalSize,
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
			return fmt.Errorf("create file record failed (compensation also failed): %w", err)
		}
		return fmt.Errorf("create file record failed, backend object cleaned up: %w", err)
	}

	return nil
}
