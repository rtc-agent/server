package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// LoopRepo Loop 仓储接口
type LoopRepo interface {
	Create(ctx context.Context, loop *model.Loop) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Loop, error)
	FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Loop, error)
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Loop, error)
}

type loopRepo struct {
	db *gorm.DB
}

// NewLoopRepo 创建 LoopRepo
func NewLoopRepo(db *gorm.DB) LoopRepo {
	return &loopRepo{db: db}
}

func (r *loopRepo) Create(ctx context.Context, loop *model.Loop) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(loop).Error; err != nil {
		return fmt.Errorf("create loop: %w", err)
	}
	return nil
}

func (r *loopRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Loop, error) {
	var loop model.Loop
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&loop, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get loop %s: %w", id, ErrLoopNotFound)
		}
		return nil, fmt.Errorf("get loop %s: %w", id, err)
	}
	return &loop, nil
}

// FindActive 查找指定 session 的 active loop，不存在返回 (nil, nil)
func (r *loopRepo) FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Loop, error) {
	var loop model.Loop
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND status = ?", sessionID, string(model.LoopStatusActive)).
		Order("created_at DESC").
		First(&loop).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("find active loop for session %s: %w", sessionID, err)
	}
	return &loop, nil
}

func (r *loopRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	// 终态时自动填充 completed_at
	if _, ok := fields["status"]; ok {
		status, _ := fields["status"].(string)
		if (status == string(model.LoopStatusCompleted) ||
			status == string(model.LoopStatusCancelled) ||
			status == string(model.LoopStatusExhausted)) && fields["completed_at"] == nil {
			fields["completed_at"] = gorm.Expr("NOW()")
		}
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Loop{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return fmt.Errorf("update loop %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update loop %s: %w", id, ErrLoopNotFound)
	}
	return nil
}

func (r *loopRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Loop, error) {
	var loops []*model.Loop
	q := DBFromContext(ctx, r.db).WithContext(ctx).Where("session_id = ?", sessionID).Order("created_at DESC")
	if cursor != nil {
		q = q.Where("id < ?", *cursor)
	}
	if limit <= 0 {
		limit = 50
	}
	if err := q.Limit(limit).Find(&loops).Error; err != nil {
		return nil, fmt.Errorf("list loops for session %s: %w", sessionID, err)
	}
	return loops, nil
}

// ensure interfaces
var _ LoopRepo = (*loopRepo)(nil)
