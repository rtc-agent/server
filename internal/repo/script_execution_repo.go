package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// ScriptExecutionRepo provides CRUD operations on the script_executions table.
type ScriptExecutionRepo interface {
	// Create stores a new script execution record.
	Create(ctx context.Context, exec *model.ScriptExecution) error
	// GetByRtcID looks up a script execution by its RTC ID.
	GetByRtcID(ctx context.Context, rtcID uuid.UUID) (*model.ScriptExecution, error)
	// ListBySession lists script executions for a session with cursor pagination.
	ListBySession(ctx context.Context, sessionID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error)
	// ListByUser lists script executions for a user with cursor pagination.
	ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error)
}

type scriptExecutionRepo struct {
	db *gorm.DB
}

// NewScriptExecutionRepo creates a new ScriptExecutionRepo.
func NewScriptExecutionRepo(db *gorm.DB) ScriptExecutionRepo {
	return &scriptExecutionRepo{db: db}
}

var _ ScriptExecutionRepo = (*scriptExecutionRepo)(nil)

func (r *scriptExecutionRepo) Create(ctx context.Context, exec *model.ScriptExecution) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(exec).Error; err != nil {
		return fmt.Errorf("create script execution: %w", err)
	}
	return nil
}

func (r *scriptExecutionRepo) GetByRtcID(ctx context.Context, rtcID uuid.UUID) (*model.ScriptExecution, error) {
	var exec model.ScriptExecution
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("rtc_id = ?", rtcID).First(&exec).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrScriptExecutionNotFound
		}
		return nil, fmt.Errorf("get script execution by rtc_id: %w", err)
	}
	return &exec, nil
}

func (r *scriptExecutionRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error) {
	return r.listScriptExecutions(ctx, "session_id = ?", sessionID, limit, offset)
}

func (r *scriptExecutionRepo) ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error) {
	return r.listScriptExecutions(ctx, "user_id = ?", userID, limit, offset)
}

// listScriptExecutions is the shared implementation for ListBySession/ListByUser.
func (r *scriptExecutionRepo) listScriptExecutions(ctx context.Context, where string, id uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error) {
	var execs []*model.ScriptExecution
	var total int64

	db := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.ScriptExecution{}).
		Where(where, id)

	if err := db.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count script executions: %w", err)
	}

	if err := db.Order("created_at DESC").Limit(limit).Offset(offset).Find(&execs).Error; err != nil {
		return nil, 0, fmt.Errorf("list script executions: %w", err)
	}

	return execs, total, nil
}
