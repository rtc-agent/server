package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TurnRepo Turn 仓储接口
type TurnRepo interface {
	Create(ctx context.Context, turn *model.Turn) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Turn, error)
	FindByClientID(ctx context.Context, clientID string) (*model.Turn, error)
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Turn, error)
	FindActiveBySession(ctx context.Context, sessionID uuid.UUID) ([]*model.Turn, error)
	// FindStaleTurns finds all turns in the given statuses that are "stale"
	// (i.e., left over from a previous server crash or restart). Used for
	// crash recovery on startup.
	FindStaleTurns(ctx context.Context, statuses []string) ([]*model.Turn, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.TurnStatus, errMsg string) error
	// UpdateStatusBySession batch-updates all turns for a session that are in
	// any of the given statuses to the target status. Returns the number of
	// rows affected. Used by stopActiveTurns to cancel pending/running turns
	// as a belt-and-suspenders measure after rtc-queue CancelSession.
	UpdateStatusBySession(ctx context.Context, sessionID uuid.UUID, fromStatuses []string, toStatus protocol.TurnStatus) (int64, error)
	// UpdateStatusAndInterruptID atomically updates both status and interrupt_id
	// in a single DB write. This prevents a race condition where SubmitRtcResult
	// could read the interrupted status before InterruptID is persisted.
	UpdateStatusAndInterruptID(ctx context.Context, id uuid.UUID, status protocol.TurnStatus, interruptID string) error
	// UpdateInterruptID sets the eino interrupt ID on a turn. Used when a turn
	// is interrupted so that SubmitRtcResult can include the interrupt ID in
	// the resume work payload, enabling eino's ResumeParams to resume from the
	// exact interrupt point.
	//
	// Deprecated: Use UpdateStatusAndInterruptID for atomic updates.
	UpdateInterruptID(ctx context.Context, id uuid.UUID, interruptID string) error
	// GetByIDs 批量查询 Turn，返回 map[id]*Turn。未找到的 ID 不会出现在 map 中。
	GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Turn, error)
}

type turnRepo struct {
	db *gorm.DB
}

// NewTurnRepo 创建 TurnRepo
func NewTurnRepo(db *gorm.DB) TurnRepo {
	return &turnRepo{db: db}
}

func (r *turnRepo) Create(ctx context.Context, turn *model.Turn) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(turn).Error; err != nil {
		return fmt.Errorf("create turn: %w", err)
	}
	return nil
}

func (r *turnRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Turn, error) {
	var turn model.Turn
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&turn, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get turn %s: %w", id, ErrTurnNotFound)
		}
		return nil, fmt.Errorf("get turn %s: %w", id, err)
	}
	return &turn, nil
}

func (r *turnRepo) FindByClientID(ctx context.Context, clientID string) (*model.Turn, error) {
	if clientID == "" {
		return nil, nil
	}
	var turn model.Turn
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&turn, "client_id = ?", clientID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // Not found is not an error
		}
		return nil, fmt.Errorf("find turn by client_id %s: %w", clientID, err)
	}
	return &turn, nil
}

func (r *turnRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Turn, error) {
	var turns []*model.Turn
	q := DBFromContext(ctx, r.db).WithContext(ctx).Where("session_id = ?", sessionID).Order("created_at ASC")
	if cursor != nil {
		q = q.Where("id > ?", *cursor)
	}
	if limit <= 0 {
		limit = 50
	}
	if err := q.Limit(limit).Find(&turns).Error; err != nil {
		return nil, fmt.Errorf("list turns by session %s: %w", sessionID, err)
	}
	return turns, nil
}

// FindActiveBySession returns all pending, running, or interrupted turns for a session.
// Used by CloseSession/StopTurn to stop active turns before closing.
// 🔧 Fix: Include "interrupted" status — turns waiting for RTC/webchat answers
// were invisible to the stop flow, leaving them stuck in DB after stop.
func (r *turnRepo) FindActiveBySession(ctx context.Context, sessionID uuid.UUID) ([]*model.Turn, error) {
	var turns []*model.Turn
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND status IN ?", sessionID, []string{
			string(model.TurnStatusPending),
			string(model.TurnStatusRunning),
			string(model.TurnStatusInterrupted),
		}).
		Order("created_at ASC").
		Find(&turns).Error
	if err != nil {
		return nil, fmt.Errorf("find active turns for session %s: %w", sessionID, err)
	}
	return turns, nil
}

// FindStaleTurns finds all turns in the given statuses. Used for crash recovery
// on startup to detect turns that were left in a non-terminal state when the
// server crashed or restarted.
func (r *turnRepo) FindStaleTurns(ctx context.Context, statuses []string) ([]*model.Turn, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	var turns []*model.Turn
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("status IN ?", statuses).
		Order("created_at ASC").
		Find(&turns).Error
	if err != nil {
		return nil, fmt.Errorf("find stale turns: %w", err)
	}
	return turns, nil
}

func (r *turnRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.TurnStatus, errMsg string) error {
	updates := map[string]any{
		"status":        string(status),
		"error_message": errMsg,
	}
	if status == model.TurnStatusRunning {
		updates["started_at"] = gorm.Expr("NOW()")
	}
	if status == model.TurnStatusCompleted || status == model.TurnStatusFailed || status == model.TurnStatusCancelled || status == model.TurnStatusMerged {
		updates["completed_at"] = gorm.Expr("NOW()")
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Turn{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update turn %s status: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update turn %s status: %w", id, ErrTurnNotFound)
	}
	return nil
}

// UpdateStatusAndInterruptID atomically updates both status and interrupt_id
// in a single DB write. This prevents a race condition where SubmitRtcResult
// could read the interrupted status before InterruptID is persisted.
func (r *turnRepo) UpdateStatusAndInterruptID(ctx context.Context, id uuid.UUID, status protocol.TurnStatus, interruptID string) error {
	updates := map[string]any{
		"status":        string(status),
		"interrupt_id":  interruptID,
	}
	if status == model.TurnStatusRunning {
		updates["started_at"] = gorm.Expr("NOW()")
	}
	if status == model.TurnStatusCompleted || status == model.TurnStatusFailed || status == model.TurnStatusCancelled || status == model.TurnStatusMerged {
		updates["completed_at"] = gorm.Expr("NOW()")
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Turn{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update turn %s status and interrupt_id: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update turn %s status and interrupt_id: %w", id, ErrTurnNotFound)
	}
	return nil
}

// UpdateStatusBySession batch-updates all turns for a session whose status is
// in fromStatuses to the target status. Returns the number of rows affected.
// Used by stopActiveTurns as a belt-and-suspenders measure after rtc-queue
// CancelSession.
func (r *turnRepo) UpdateStatusBySession(ctx context.Context, sessionID uuid.UUID, fromStatuses []string, toStatus protocol.TurnStatus) (int64, error) {
	if len(fromStatuses) == 0 {
		return 0, nil
	}
	updates := map[string]any{
		"status": string(toStatus),
	}
	if toStatus == model.TurnStatusCompleted || toStatus == model.TurnStatusFailed || toStatus == model.TurnStatusCancelled || toStatus == model.TurnStatusMerged {
		updates["completed_at"] = gorm.Expr("NOW()")
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Turn{}).
		Where("session_id = ? AND status IN ?", sessionID, fromStatuses).
		Updates(updates)
	if result.Error != nil {
		return 0, fmt.Errorf("update turns for session %s to %s: %w", sessionID, toStatus, result.Error)
	}
	return result.RowsAffected, nil
}

func (r *turnRepo) UpdateInterruptID(ctx context.Context, id uuid.UUID, interruptID string) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Turn{}).Where("id = ?", id).Update("interrupt_id", interruptID)
	if result.Error != nil {
		return fmt.Errorf("update turn %s interrupt_id: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update turn %s interrupt_id: %w", id, ErrTurnNotFound)
	}
	return nil
}

func (r *turnRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Turn, error) {
	if len(ids) == 0 {
		return make(map[uuid.UUID]*model.Turn), nil
	}
	var turns []*model.Turn
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id IN ?", ids).Find(&turns).Error; err != nil {
		return nil, fmt.Errorf("get turns by ids: %w", err)
	}
	result := make(map[uuid.UUID]*model.Turn, len(turns))
	for _, t := range turns {
		result[t.ID] = t
	}
	return result, nil
}
