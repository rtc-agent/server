package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// GoalRepo provides Goal persistence operations.
type GoalRepo interface {
	// Create stores a new Goal record.
	Create(ctx context.Context, goal *model.Goal) error
	// GetByID looks up a Goal by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.Goal, error)
	// FindActive returns the active goal for a session, or (nil, nil) if none.
	FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Goal, error)
	// Update modifies specific fields of a Goal.
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
	// ListBySession lists Goals for a session with cursor pagination.
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Goal, error)
}

type goalRepo struct {
	db *gorm.DB
}

// NewGoalRepo creates a new GoalRepo.
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

// FindActive returns the active goal for a session, or (nil, nil) if none exists.
func (r *goalRepo) FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Goal, error) {
	var goal model.Goal
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND status = ?", sessionID, model.GoalStatusActive).
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
	autoFillCompletedAt(fields, goalTerminalStatuses)
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
	return listBySessionPaged[model.Goal](ctx, r.db, sessionID, cursor, limit, "created_at DESC, id DESC", "id", "<", "goals")
}

// Ensure interface compliance.
var _ GoalRepo = (*goalRepo)(nil)
