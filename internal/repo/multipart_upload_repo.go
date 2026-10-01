package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// MultipartUploadRepo provides multipart upload persistence operations.
type MultipartUploadRepo interface {
	// Create stores a new multipart upload record.
	Create(ctx context.Context, upload *model.MultipartUpload) error
	// GetByID looks up a multipart upload by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.MultipartUpload, error)
	// GetByUploadID looks up a multipart upload by upload_id (MinIO upload ID).
	GetByUploadID(ctx context.Context, uploadID string) (*model.MultipartUpload, error)
	// UpdateStatus updates the upload status and uploaded parts count.
	UpdateStatus(ctx context.Context, id uuid.UUID, status string, uploadedParts int) error
	// FindExpired finds uploads that expired before the given time.
	FindExpired(ctx context.Context, before time.Time) ([]*model.MultipartUpload, error)
	// Delete deletes a multipart upload record.
	Delete(ctx context.Context, id uuid.UUID) error
	// CountActiveByUser counts active uploads (status = "uploading") for a user.
	CountActiveByUser(ctx context.Context, userID string) (int64, error)

	// Part operations

	// CreatePart stores a new part record.
	CreatePart(ctx context.Context, part *model.MultipartUploadPart) error
	// ListParts lists all parts for an upload.
	ListParts(ctx context.Context, uploadID uuid.UUID) ([]*model.MultipartUploadPart, error)
	// DeleteParts deletes all parts for an upload.
	DeleteParts(ctx context.Context, uploadID uuid.UUID) error
}

type multipartUploadRepo struct {
	db *gorm.DB
}

// NewMultipartUploadRepo creates a new MultipartUploadRepo.
func NewMultipartUploadRepo(db *gorm.DB) MultipartUploadRepo {
	return &multipartUploadRepo{db: db}
}

func (r *multipartUploadRepo) Create(ctx context.Context, upload *model.MultipartUpload) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(upload).Error; err != nil {
		return fmt.Errorf("create multipart upload %s: %w", upload.ID, err)
	}
	return nil
}

func (r *multipartUploadRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.MultipartUpload, error) {
	var upload model.MultipartUpload
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id = ?", id).First(&upload).Error
	if err != nil {
		return nil, fmt.Errorf("get multipart upload %s: %w", id, err)
	}
	return &upload, nil
}

func (r *multipartUploadRepo) GetByUploadID(ctx context.Context, uploadID string) (*model.MultipartUpload, error) {
	var upload model.MultipartUpload
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("upload_id = ?", uploadID).First(&upload).Error
	if err != nil {
		return nil, fmt.Errorf("get multipart upload by upload_id %s: %w", uploadID, err)
	}
	return &upload, nil
}

func (r *multipartUploadRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status string, uploadedParts int) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.MultipartUpload{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":         status,
			"uploaded_parts": uploadedParts,
		}).Error; err != nil {
		return fmt.Errorf("update multipart upload %s status: %w", id, err)
	}
	return nil
}

func (r *multipartUploadRepo) FindExpired(ctx context.Context, before time.Time) ([]*model.MultipartUpload, error) {
	var uploads []*model.MultipartUpload
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("status = ? AND expires_at < ?", "uploading", before).
		Find(&uploads).Error; err != nil {
		return nil, fmt.Errorf("find expired multipart uploads before %v: %w", before, err)
	}
	return uploads, nil
}

func (r *multipartUploadRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Delete(&model.MultipartUpload{}, "id = ?", id).Error; err != nil {
		return fmt.Errorf("delete multipart upload %s: %w", id, err)
	}
	return nil
}

func (r *multipartUploadRepo) CountActiveByUser(ctx context.Context, userID string) (int64, error) {
	var count int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.MultipartUpload{}).
		Where("user_id = ? AND status = ?", userID, "uploading").
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count active multipart uploads by user %s: %w", userID, err)
	}
	return count, nil
}

// Part operations

func (r *multipartUploadRepo) CreatePart(ctx context.Context, part *model.MultipartUploadPart) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(part).Error; err != nil {
		return fmt.Errorf("create multipart upload part %d: %w", part.PartNumber, err)
	}
	return nil
}

func (r *multipartUploadRepo) ListParts(ctx context.Context, uploadID uuid.UUID) ([]*model.MultipartUploadPart, error) {
	var parts []*model.MultipartUploadPart
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("upload_id = ?", uploadID).
		Order("part_number ASC").
		Find(&parts).Error; err != nil {
		return nil, fmt.Errorf("list parts for upload %s: %w", uploadID, err)
	}
	return parts, nil
}

func (r *multipartUploadRepo) DeleteParts(ctx context.Context, uploadID uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("upload_id = ?", uploadID).
		Delete(&model.MultipartUploadPart{}).Error; err != nil {
		return fmt.Errorf("delete parts for upload %s: %w", uploadID, err)
	}
	return nil
}
