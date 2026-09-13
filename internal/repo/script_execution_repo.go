package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// ScriptExecutionRepo 提供 script_executions 表的 CRUD 操作。
type ScriptExecutionRepo interface {
	Create(ctx context.Context, exec *model.ScriptExecution) error
	GetByRtcID(ctx context.Context, rtcID uuid.UUID) (*model.ScriptExecution, error)
	ListBySession(ctx context.Context, sessionID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error)
	ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error)
}

type scriptExecutionRepo struct {
	db *gorm.DB
}

// NewScriptExecutionRepo 创建 ScriptExecutionRepo
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
	var execs []*model.ScriptExecution
	var total int64

	db := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.ScriptExecution{}).
		Where("session_id = ?", sessionID)

	if err := db.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count script executions: %w", err)
	}

	if err := db.Order("created_at DESC").Limit(limit).Offset(offset).Find(&execs).Error; err != nil {
		return nil, 0, fmt.Errorf("list script executions: %w", err)
	}

	return execs, total, nil
}

func (r *scriptExecutionRepo) ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*model.ScriptExecution, int64, error) {
	var execs []*model.ScriptExecution
	var total int64

	db := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.ScriptExecution{}).
		Where("user_id = ?", userID)

	if err := db.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count script executions: %w", err)
	}

	if err := db.Order("created_at DESC").Limit(limit).Offset(offset).Find(&execs).Error; err != nil {
		return nil, 0, fmt.Errorf("list script executions: %w", err)
	}

	return execs, total, nil
}
