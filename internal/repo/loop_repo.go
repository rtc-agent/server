package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// LoopRepo provides Loop persistence operations.
type LoopRepo interface {
	// Create stores a new Loop record.
	Create(ctx context.Context, loop *model.Loop) error
	// GetByID looks up a Loop by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.Loop, error)
	// FindActive returns the active loop for a session, or (nil, nil) if none.
	FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Loop, error)
	// Update modifies specific fields of a Loop.
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
	// ListBySession lists Loops for a session with cursor pagination.
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Loop, error)
	// FindStaleLoops returns active loops that lack an asynq task and have not
	// run recently (before staleThreshold). These are loops whose scheduled
	// task was lost (e.g., after server restart).
	FindStaleLoops(ctx context.Context, staleThreshold time.Time) ([]*model.Loop, error)
	// FindExpiredLoops returns active loops whose expires_at has passed.
	FindExpiredLoops(ctx context.Context) ([]*model.Loop, error)
}

type loopRepo struct {
	db *gorm.DB
}

// NewLoopRepo creates a new LoopRepo.
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

// FindActive returns the active loop for a session, or (nil, nil) if none exists.
func (r *loopRepo) FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Loop, error) {
	var loop model.Loop
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND status = ?", sessionID, model.LoopStatusActive).
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
	return updateWithStateGuard(ctx, r.db, &model.Loop{}, id, fields, loopTerminalStatuses,
		"loop", ErrLoopNotFound)
}

func (r *loopRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Loop, error) {
	return listBySessionPaged[model.Loop](ctx, r.db, sessionID, cursor, limit, "created_at DESC, id DESC", "id", "<", "loops")
}

// FindStaleLoops returns active loops that have not run recently (before staleThreshold).
// These are loops whose scheduled task was lost (e.g., after server restart) or whose
// worker crashed before completing the task.
//
// Query conditions explained:
//   - status = active: only consider loops that should be running
//   - last_run_at IS NOT NULL: loop has run at least once
//   - last_run_at + interval_seconds + stale_threshold < now(): the loop should have
//     run again by now (considering its interval) but hasn't, indicating it's truly stale
//
// Note: We no longer check asynq_task_id because we use loop.ID as the task ID
// for idempotent scheduling. The task ID is always set and doesn't indicate
// whether a task is currently pending or being processed. The distributed lock
// in HandleLoopTask prevents concurrent execution.
// Safety limit of 100 loops to prevent overwhelming the recovery process.
func (r *loopRepo) FindStaleLoops(ctx context.Context, staleThreshold time.Time) ([]*model.Loop, error) {
	var loops []*model.Loop
	const safetyLimit = 100
	// Calculate the cutoff time: last_run_at + interval_seconds should be before staleThreshold
	// This ensures we don't mark a loop as stale if it's still within its normal execution interval
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("status = ? AND last_run_at IS NOT NULL AND "+
			"last_run_at + (interval_seconds * interval '1 second') < ?",
			model.LoopStatusActive, staleThreshold).
		Limit(safetyLimit).
		Find(&loops).Error
	if err != nil {
		return nil, fmt.Errorf("find stale loops: %w", err)
	}
	return loops, nil
}

// FindExpiredLoops returns active loops whose expires_at has passed.
// Safety limit of 100 loops to prevent overwhelming the cleanup process.
func (r *loopRepo) FindExpiredLoops(ctx context.Context) ([]*model.Loop, error) {
	var loops []*model.Loop
	const safetyLimit = 100
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("status = ? AND expires_at IS NOT NULL AND expires_at < ?",
			model.LoopStatusActive, time.Now()).
		Limit(safetyLimit).
		Find(&loops).Error
	if err != nil {
		return nil, fmt.Errorf("find expired loops: %w", err)
	}
	return loops, nil
}

// Ensure interface compliance.
var _ LoopRepo = (*loopRepo)(nil)
