package repo

import (
	"context"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// FileRepo provides file metadata persistence operations.
type FileRepo interface {
	// Create stores a new file record.
	Create(ctx context.Context, file *model.File) error
	// GetByUserAndKey looks up a file by user_id and key.
	GetByUserAndKey(ctx context.Context, userID string, key string) (*model.File, error)
	// Update modifies a file record.
	Update(ctx context.Context, file *model.File) error
	// Delete soft-deletes a file by ID.
	Delete(ctx context.Context, id uuid.UUID) error
	// DeleteByUserAndKey soft-deletes a file by user_id and key.
	DeleteByUserAndKey(ctx context.Context, userID string, key string) error
	// SumSizeByUser returns total file size for a user.
	SumSizeByUser(ctx context.Context, userID string) (int64, error)
	// SumSizeByUserGrouped returns total file size grouped by user_id.
	// Used by ReconcileQuota (1H-3 R7 M2) to compare DB truth with Redis counter.
	// SQL: SELECT user_id, COALESCE(SUM(size), 0) FROM files GROUP BY user_id
	SumSizeByUserGrouped(ctx context.Context) (map[string]int64, error)
}

type fileRepo struct {
	db *gorm.DB
}

// NewFileRepo creates a new FileRepo.
func NewFileRepo(db *gorm.DB) FileRepo {
	return &fileRepo{db: db}
}

func (r *fileRepo) Create(ctx context.Context, file *model.File) error {
	return r.db.WithContext(ctx).Create(file).Error
}

func (r *fileRepo) GetByUserAndKey(ctx context.Context, userID string, key string) (*model.File, error) {
	var file model.File
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND key = ?", userID, key).
		First(&file).Error
	if err != nil {
		return nil, err
	}
	return &file, nil
}

func (r *fileRepo) Update(ctx context.Context, file *model.File) error {
	return r.db.WithContext(ctx).Save(file).Error
}

func (r *fileRepo) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).
		Delete(&model.File{}, "id = ?", id).Error
}

func (r *fileRepo) DeleteByUserAndKey(ctx context.Context, userID string, key string) error {
	return r.db.WithContext(ctx).
		Where("user_id = ? AND key = ?", userID, key).
		Delete(&model.File{}).Error
}

func (r *fileRepo) SumSizeByUser(ctx context.Context, userID string) (int64, error) {
	var total int64
	err := r.db.WithContext(ctx).
		Model(&model.File{}).
		Where("user_id = ?", userID).
		Select("COALESCE(SUM(size), 0)").
		Scan(&total).Error
	return total, err
}

func (r *fileRepo) SumSizeByUserGrouped(ctx context.Context) (map[string]int64, error) {
	type result struct {
		UserID string
		Total  int64
	}
	var results []result
	err := r.db.WithContext(ctx).
		Model(&model.File{}).
		Select("user_id, COALESCE(SUM(size), 0) as total").
		Group("user_id").
		Scan(&results).Error
	if err != nil {
		return nil, err
	}

	m := make(map[string]int64, len(results))
	for _, r := range results {
		m[r.UserID] = r.Total
	}
	return m, nil
}
