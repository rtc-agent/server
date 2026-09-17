package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// UserMemoryRepo provides user memory persistence operations.
type UserMemoryRepo interface {
	// Create stores a new user memory record.
	Create(ctx context.Context, memory *model.UserMemory) error

	// GetByID looks up a user memory by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.UserMemory, error)

	// ListByUser lists all memories for a user.
	ListByUser(ctx context.Context, userID uuid.UUID, limit int) ([]*model.UserMemory, error)

	// ListByCategory lists memories for a user filtered by category.
	ListByCategory(ctx context.Context, userID uuid.UUID, category string, limit int) ([]*model.UserMemory, error)

	// Update modifies specific fields of a user memory.
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// Delete performs a soft delete on a user memory.
	Delete(ctx context.Context, id uuid.UUID) error

	// IncrementAccessCount increments the access count and updates last_accessed_at.
	IncrementAccessCount(ctx context.Context, id uuid.UUID) error

	// SearchByKeyword performs a keyword search across tags, title, and content.
	SearchByKeyword(ctx context.Context, userID uuid.UUID, query string, limit int) ([]*model.UserMemory, error)

	// CountByUser returns the total memory count for a user.
	CountByUser(ctx context.Context, userID uuid.UUID) (int, error)
}

type userMemoryRepo struct {
	db *gorm.DB
}

// NewUserMemoryRepo creates a new UserMemoryRepo.
func NewUserMemoryRepo(db *gorm.DB) UserMemoryRepo {
	return &userMemoryRepo{db: db}
}

func (r *userMemoryRepo) Create(ctx context.Context, memory *model.UserMemory) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(memory).Error; err != nil {
		return fmt.Errorf("create user memory: %w", err)
	}
	return nil
}

func (r *userMemoryRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.UserMemory, error) {
	var memory model.UserMemory
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&memory, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("user memory %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get user memory %s: %w", id, err)
	}
	return &memory, nil
}

func (r *userMemoryRepo) ListByUser(ctx context.Context, userID uuid.UUID, limit int) ([]*model.UserMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	var memories []*model.UserMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("updated_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("list user memories by user %s: %w", userID, err)
	}
	return memories, nil
}

func (r *userMemoryRepo) ListByCategory(ctx context.Context, userID uuid.UUID, category string, limit int) ([]*model.UserMemory, error) {
	return listByCategory[model.UserMemory](ctx, r.db, "user_id", userID, category, limit, "updated_at DESC", "AND deleted_at IS NULL", "user memories")
}

func (r *userMemoryRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	return updateWithAutoTimestamp(ctx, r.db, &model.UserMemory{}, id, fields, "id = ? AND deleted_at IS NULL", "user memory", ErrNotFound)
}

func (r *userMemoryRepo) Delete(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Update("deleted_at", now)
	if result.Error != nil {
		return fmt.Errorf("delete user memory %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("user memory %s: %w", id, ErrNotFound)
	}
	return nil
}

func (r *userMemoryRepo) IncrementAccessCount(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{
			"access_count":     gorm.Expr("access_count + 1"),
			"last_accessed_at": now,
		})
	if result.Error != nil {
		return fmt.Errorf("increment access count for user memory %s: %w", id, result.Error)
	}
	return nil
}

// SearchByKeyword performs a keyword search across tags, title, and content
// using PostgreSQL ILIKE for case-insensitive matching.
func (r *userMemoryRepo) SearchByKeyword(ctx context.Context, userID uuid.UUID, query string, limit int) ([]*model.UserMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	likeQuery := "%" + escapeLikePattern(query) + "%"

	var memories []*model.UserMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Where("(title ILIKE ? OR content ILIKE ? OR description ILIKE ? OR tags::text ILIKE ?)",
			likeQuery, likeQuery, likeQuery, likeQuery).
		Order("updated_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("search user memories by keyword: %w", err)
	}
	return memories, nil
}

func (r *userMemoryRepo) CountByUser(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int64
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("count user memories by user %s: %w", userID, err)
	}
	return int(count), nil
}
