package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	// SumSizeByUserGroupedPaginated returns total file size grouped by user_id,
	// paginated by user_id cursor. Returns (map, nextCursor, error).
	// An empty cursor starts from the beginning; an empty nextCursor in the result
	// means no more pages.
	// SQL: SELECT user_id, COALESCE(SUM(size), 0) as total
	//      FROM files WHERE user_id > ? GROUP BY user_id ORDER BY user_id LIMIT ?
	SumSizeByUserGroupedPaginated(ctx context.Context, cursor string, limit int) (map[string]int64, string, error)
}

type fileRepo struct {
	db *gorm.DB
}

// NewFileRepo creates a new FileRepo.
func NewFileRepo(db *gorm.DB) FileRepo {
	return &fileRepo{db: db}
}

func (r *fileRepo) Create(ctx context.Context, file *model.File) error {
	// Use upsert to handle the case where a soft-deleted record with the same
	// key exists. This restores the record by clearing deleted_at
	// and updating other fields.
	// Explicit column list avoids overwriting immutable fields (id, created_at,
	// user_id, key) which UpdateAll: true would inadvertently update.
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"bucket", "size", "content_type", "e_tag", "updated_at", "deleted_at",
			}),
		}).
		Create(file).Error; err != nil {
		return fmt.Errorf("create file %s: %w", file.Key, err)
	}
	return nil
}

// GetByUserAndKey looks up a file by user_id and key.
// Returns (nil, nil) when the record is not found — this deviates from the
// typical repo pattern of returning a sentinel error, because the instant
// upload path needs to distinguish "not found" from "database error" without
// calling errors.Is on every call site.
func (r *fileRepo) GetByUserAndKey(ctx context.Context, userID string, key string) (*model.File, error) {
	var file model.File
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND key = ?", userID, key).
		First(&file).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // not found is not an error — see method doc
		}
		return nil, fmt.Errorf("get file by user %s and key %s: %w", userID, key, err)
	}
	return &file, nil
}

func (r *fileRepo) Update(ctx context.Context, file *model.File) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Save(file).Error; err != nil {
		return fmt.Errorf("update file %s: %w", file.Key, err)
	}
	return nil
}

func (r *fileRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Delete(&model.File{}, "id = ?", id).Error; err != nil {
		return fmt.Errorf("delete file %s: %w", id, err)
	}
	return nil
}

func (r *fileRepo) DeleteByUserAndKey(ctx context.Context, userID string, key string) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND key = ?", userID, key).
		Delete(&model.File{}).Error; err != nil {
		return fmt.Errorf("delete file by user %s and key %s: %w", userID, key, err)
	}
	return nil
}

func (r *fileRepo) SumSizeByUser(ctx context.Context, userID string) (int64, error) {
	var total int64
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.File{}).
		Where("user_id = ?", userID).
		Select("COALESCE(SUM(size), 0)").
		Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("sum file size by user %s: %w", userID, err)
	}
	return total, nil
}

func (r *fileRepo) SumSizeByUserGrouped(ctx context.Context) (map[string]int64, error) {
	type result struct {
		UserID string
		Total  int64
	}
	var results []result
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.File{}).
		Select("user_id, COALESCE(SUM(size), 0) as total").
		Group("user_id").
		Scan(&results).Error; err != nil {
		return nil, fmt.Errorf("sum file size grouped by user: %w", err)
	}

	m := make(map[string]int64, len(results))
	for _, r := range results {
		m[r.UserID] = r.Total
	}
	return m, nil
}

func (r *fileRepo) SumSizeByUserGroupedPaginated(ctx context.Context, cursor string, limit int) (map[string]int64, string, error) {
	type result struct {
		UserID string
		Total  int64
	}
	var results []result
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.File{}).
		Where("user_id > ?", cursor).
		Select("user_id, COALESCE(SUM(size), 0) as total").
		Group("user_id").
		Order("user_id").
		Limit(limit).
		Scan(&results).Error; err != nil {
		return nil, "", fmt.Errorf("sum file size grouped by user paginated: %w", err)
	}

	m := make(map[string]int64, len(results))
	var nextCursor string
	for _, r := range results {
		m[r.UserID] = r.Total
		nextCursor = r.UserID // last row becomes the next cursor
	}

	// If we got fewer rows than the limit, there are no more pages.
	if len(results) < limit {
		nextCursor = ""
	}
	return m, nextCursor, nil
}
