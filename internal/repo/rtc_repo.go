package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"

	"gorm.io/gorm"
)

// RtcRepo RTC 仓储接口
type RtcRepo interface {
	Create(ctx context.Context, rtc *model.Rtc) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Rtc, error)
	FindByClientID(ctx context.Context, clientID string) (*model.Rtc, error)
	ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Rtc, error)
	ListByTurn(ctx context.Context, turnID uuid.UUID) ([]*model.Rtc, error)
	ListByMessageIDs(ctx context.Context, messageIDs []uuid.UUID) ([]*model.Rtc, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.RtcStatus) error
	UpdateResult(ctx context.Context, id uuid.UUID, status protocol.RtcStatus, result *string, errMsg string) error
	UpdateOutputMessageID(ctx context.Context, id uuid.UUID, outputMessageID uuid.UUID) error
	// GetByIDs 批量查询 RTC，返回 map[id]*Rtc。未找到的 ID 不会出现在 map 中。
	GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Rtc, error)
}

type rtcRepo struct {
	db *gorm.DB
}

// NewRtcRepo 创建 RtcRepo
func NewRtcRepo(db *gorm.DB) RtcRepo {
	return &rtcRepo{db: db}
}

func (r *rtcRepo) Create(ctx context.Context, rtc *model.Rtc) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(rtc).Error; err != nil {
		return fmt.Errorf("create rtc: %w", err)
	}
	return nil
}

func (r *rtcRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Rtc, error) {
	var rtc model.Rtc
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&rtc, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get rtc %s: %w", id, ErrRtcNotFound)
		}
		return nil, fmt.Errorf("get rtc %s: %w", id, err)
	}
	return &rtc, nil
}

func (r *rtcRepo) FindByClientID(ctx context.Context, clientID string) (*model.Rtc, error) {
	if clientID == "" {
		return nil, nil
	}
	var rtc model.Rtc
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&rtc, "client_id = ?", clientID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // Not found is not an error
		}
		return nil, fmt.Errorf("find rtc by client_id %s: %w", clientID, err)
	}
	return &rtc, nil
}

func (r *rtcRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Rtc, error) {
	var rtcs []*model.Rtc
	q := DBFromContext(ctx, r.db).WithContext(ctx).Where("session_id = ?", sessionID).Order("created_at ASC")
	if cursor != nil {
		q = q.Where("id > ?", *cursor)
	}
	if limit <= 0 {
		limit = 50
	}
	if err := q.Limit(limit).Find(&rtcs).Error; err != nil {
		return nil, fmt.Errorf("list rtcs by session %s: %w", sessionID, err)
	}
	return rtcs, nil
}

func (r *rtcRepo) ListByTurn(ctx context.Context, turnID uuid.UUID) ([]*model.Rtc, error) {
	var rtcs []*model.Rtc
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("turn_id = ?", turnID).Order("offset ASC").Find(&rtcs).Error
	if err != nil {
		return nil, fmt.Errorf("list rtcs by turn %s: %w", turnID, err)
	}
	return rtcs, nil
}

func (r *rtcRepo) ListByMessageIDs(ctx context.Context, messageIDs []uuid.UUID) ([]*model.Rtc, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	var rtcs []*model.Rtc
	err := DBFromContext(ctx, r.db).WithContext(ctx).Where("message_id IN ?", messageIDs).Find(&rtcs).Error
	if err != nil {
		return nil, fmt.Errorf("list rtcs by message_ids: %w", err)
	}
	return rtcs, nil
}

func (r *rtcRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.RtcStatus) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Rtc{}).Where("id = ?", id).Update("status", string(status))
	if result.Error != nil {
		return fmt.Errorf("update rtc %s status: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update rtc %s status: %w", id, ErrRtcNotFound)
	}
	return nil
}

func (r *rtcRepo) UpdateResult(ctx context.Context, id uuid.UUID, status protocol.RtcStatus, result *string, errMsg string) error {
	updates := map[string]any{
		"status":        string(status),
		"error_message": errMsg,
		"completed_at":  gorm.Expr("NOW()"),
	}
	if result != nil {
		updates["result"] = model.JSONBString(*result)
	} else {
		updates["result"] = nil
	}
	dbResult := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Rtc{}).
		Where("id = ?", id).
		Updates(updates)
	if dbResult.Error != nil {
		return fmt.Errorf("update rtc %s result: %w", id, dbResult.Error)
	}
	if dbResult.RowsAffected == 0 {
		return fmt.Errorf("update rtc %s result: %w", id, ErrRtcNotFound)
	}
	return nil
}

func (r *rtcRepo) UpdateOutputMessageID(ctx context.Context, id uuid.UUID, outputMessageID uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Rtc{}).
		Where("id = ?", id).
		Update("output_message_id", outputMessageID)
	if result.Error != nil {
		return fmt.Errorf("update rtc %s output_message_id: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update rtc %s output_message_id: %w", id, ErrRtcNotFound)
	}
	return nil
}

func (r *rtcRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Rtc, error) {
	if len(ids) == 0 {
		return make(map[uuid.UUID]*model.Rtc), nil
	}
	var rtcs []*model.Rtc
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id IN ?", ids).Find(&rtcs).Error; err != nil {
		return nil, fmt.Errorf("get rtcs by ids: %w", err)
	}
	result := make(map[uuid.UUID]*model.Rtc, len(rtcs))
	for _, r := range rtcs {
		result[r.ID] = r
	}
	return result, nil
}
