package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// GoalRepo Goal 仓储接口
type GoalRepo interface {
	Create(ctx context.Context, goal *model.Goal) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Goal, error)
	FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Goal, error)
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Goal, error)
}

type goalRepo struct {
	db *gorm.DB
}

// NewGoalRepo 创建 GoalRepo
func NewGoalRepo(db *gorm.DB) GoalRepo {
	return &goalRepo{db: db}
}

func (r *goalRepo) Create(ctx context.Context, goal *model.Goal) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(goal).Error; err != nil {
		return fmt.Errorf("create goal: %w", err)
	}
	return nil
}

func (r *goalRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Goal, error) {
	var goal model.Goal
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&goal, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get goal %s: %w", id, ErrGoalNotFound)
		}
		return nil, fmt.Errorf("get goal %s: %w", id, err)
	}
	return &goal, nil
}

// FindActive 查找指定 session 的 active goal，不存在返回 (nil, nil)
func (r *goalRepo) FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Goal, error) {
	var goal model.Goal
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND status = ?", sessionID, string(model.GoalStatusActive)).
		Order("created_at DESC").
		First(&goal).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("find active goal for session %s: %w", sessionID, err)
	}
	return &goal, nil
}

func (r *goalRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	// 终态时自动填充 completed_at
	if _, ok := fields["status"]; ok {
		status, _ := fields["status"].(string)
		if (status == string(model.GoalStatusCompleted) ||
			status == string(model.GoalStatusCancelled) ||
			status == string(model.GoalStatusExhausted)) && fields["completed_at"] == nil {
			fields["completed_at"] = gorm.Expr("NOW()")
		}
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Goal{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return fmt.Errorf("update goal %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update goal %s: %w", id, ErrGoalNotFound)
	}
	return nil
}

func (r *goalRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Goal, error) {
	var goals []*model.Goal
	q := DBFromContext(ctx, r.db).WithContext(ctx).Where("session_id = ?", sessionID).Order("created_at DESC")
	if cursor != nil {
		q = q.Where("id < ?", *cursor)
	}
	if limit <= 0 {
		limit = 50
	}
	if err := q.Limit(limit).Find(&goals).Error; err != nil {
		return nil, fmt.Errorf("list goals for session %s: %w", sessionID, err)
	}
	return goals, nil
}

// ensure interfaces
var _ GoalRepo = (*goalRepo)(nil)
