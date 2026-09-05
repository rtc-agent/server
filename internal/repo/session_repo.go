package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"

	"gorm.io/gorm"
)

// SessionRepo 会话仓储接口
type SessionRepo interface {
	Create(ctx context.Context, session *model.Session) error
	GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error)
	FindByClientID(ctx context.Context, clientID string) (*model.Session, error)
	GetByUser(ctx context.Context, userID uuid.UUID, cursor *string, limit int) ([]*model.Session, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.SessionStatus) error
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
	// TouchActive 原子更新活跃会话的 updated_at；若会话不存在或已关闭返回错误。
	// 用于在创建新 turn/message 时并发安全地"占位"，避免 TOCTOU 竞态。
	TouchActive(ctx context.Context, id uuid.UUID) error
	// UpdateFieldsActive 原子更新活跃会话的指定字段（自动带 updated_at）；
	// 若会话不存在或已关闭返回错误。
	UpdateFieldsActive(ctx context.Context, id uuid.UUID, fields map[string]any) error
	// GetByIDs 批量查询会话，返回 map[id]*Session。未找到的 ID 不会出现在 map 中。
	GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Session, error)
}

type sessionRepo struct {
	db *gorm.DB
}

// NewSessionRepo 创建 SessionRepo
func NewSessionRepo(db *gorm.DB) SessionRepo {
	return &sessionRepo{db: db}
}

func (r *sessionRepo) Create(ctx context.Context, session *model.Session) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(session).Error; err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *sessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error) {
	var session model.Session
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&session, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get session %s: %w", id, ErrSessionNotFound)
		}
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}
	return &session, nil
}

func (r *sessionRepo) FindByClientID(ctx context.Context, clientID string) (*model.Session, error) {
	if clientID == "" {
		return nil, nil
	}
	var session model.Session
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&session, "client_id = ?", clientID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // Not found is not an error
		}
		return nil, fmt.Errorf("find session by client_id %s: %w", clientID, err)
	}
	return &session, nil
}

func (r *sessionRepo) GetByUser(ctx context.Context, userID uuid.UUID, cursor *string, limit int) ([]*model.Session, error) {
	var sessions []*model.Session
	q := DBFromContext(ctx, r.db).WithContext(ctx).Where("owner_kind = ? AND owner_ref_id = ?", "user", userID.String()).Order("created_at DESC")
	if cursor != nil {
		q = q.Where("id < ?", *cursor)
	}
	if limit <= 0 {
		limit = 20
	}
	if err := q.Limit(limit).Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("get sessions by user %s: %w", userID, err)
	}
	return sessions, nil
}

func (r *sessionRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.SessionStatus) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     string(status),
			"updated_at": time.Now(),
			"closed_at":  gorm.Expr("CASE WHEN ? = ? THEN NOW() ELSE closed_at END", status, model.SessionStatusClosed),
		})
	if result.Error != nil {
		return fmt.Errorf("update session %s status: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update session %s status: %w", id, ErrSessionNotFound)
	}
	return nil
}

func (r *sessionRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	fields["updated_at"] = time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.Session{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return fmt.Errorf("update session %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update session %s: %w", id, ErrSessionNotFound)
	}
	return nil
}

func (r *sessionRepo) TouchActive(ctx context.Context, id uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ? AND status != ?", id, model.SessionStatusClosed).
		Update("updated_at", time.Now())
	if result.Error != nil {
		return fmt.Errorf("touch session %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("touch session %s: %w", id, ErrSessionClosedOrNotFound)
	}
	return nil
}

func (r *sessionRepo) UpdateFieldsActive(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	fields["updated_at"] = time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ? AND status != ?", id, model.SessionStatusClosed).
		Updates(fields)
	if result.Error != nil {
		return fmt.Errorf("update session %s fields: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update session %s fields: %w", id, ErrSessionClosedOrNotFound)
	}
	return nil
}

func (r *sessionRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Session, error) {
	if len(ids) == 0 {
		return make(map[uuid.UUID]*model.Session), nil
	}
	var sessions []*model.Session
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Where("id IN ?", ids).Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("get sessions by ids: %w", err)
	}
	result := make(map[uuid.UUID]*model.Session, len(sessions))
	for _, s := range sessions {
		result[s.ID] = s
	}
	return result, nil
}
