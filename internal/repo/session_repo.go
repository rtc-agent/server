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

// SessionRepo provides session persistence operations.
type SessionRepo interface {
	// Create stores a new session record.
	Create(ctx context.Context, session *model.Session) error
	// GetByID looks up a session by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error)
	// FindByClientID looks up a session by client-assigned ID.
	FindByClientID(ctx context.Context, clientID string) (*model.Session, error)
	// GetByUser lists sessions for a user with cursor pagination.
	GetByUser(ctx context.Context, userID uuid.UUID, cursor *string, limit int) ([]*model.Session, error)
	// UpdateStatus updates the session status.
	UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.SessionStatus) error
	// Update modifies specific fields of a session.
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error
	// TouchActive atomically updates updated_at for an active session;
	// returns an error if the session is missing or closed. Used to
	// "claim" a session concurrently when creating a new turn/message,
	// avoiding TOCTOU races.
	TouchActive(ctx context.Context, id uuid.UUID) error
	// UpdateFieldsActive atomically updates the given fields (with automatic
	// updated_at) for an active session; returns an error if the session
	// is missing or closed.
	UpdateFieldsActive(ctx context.Context, id uuid.UUID, fields map[string]any) error
	// GetByIDs batch-fetches sessions, returning map[id]*Session. Missing IDs are omitted.
	GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Session, error)
	// ListByRoot lists all descendant sessions by root_server_session_id,
	// filtered by the given status, excluding the root itself.
	ListByRoot(ctx context.Context, rootServerSessionID uuid.UUID, status string) ([]*model.Session, error)
	// FindActiveByParent finds active child sessions (status != closed) by
	// parent_server_session_id. Used for cascade cancel: when a parent
	// session is cancelled, all active sub-agents must be cancelled too.
	FindActiveByParent(ctx context.Context, parentSessionID uuid.UUID) ([]*model.Session, error)
	// AtomicAddTokenUsage atomically increments the session's token usage counters.
	AtomicAddTokenUsage(ctx context.Context, sessionID uuid.UUID, delta TokenUsageDelta) error
	// AtomicUpdateEWMA atomically updates the session's token estimate EWMA value.
	AtomicUpdateEWMA(ctx context.Context, sessionID uuid.UUID, ewma float64) error
}

// TokenUsageDelta represents incremental token usage changes.
type TokenUsageDelta struct {
	InputDelta       int64
	OutputDelta      int64
	TotalDelta       int64
	CachedReadDelta  int64
	CachedWriteDelta int64
	ReasoningDelta   int64
	CostMicrosDelta  int64

	// SetEWMA, if > 0, atomically sets token_estimate_ewma to this value.
	// Used by TokenEstimator to persist the EWMA after each LLM call.
	SetEWMA float64

	// SetCurrentContextTokens, if > 0, atomically sets current_context_tokens.
	// Used by token_callback to update the current context size after each
	// LLM call, and by persistCompressedMessages / compact to write the
	// accurate post-compaction token count.
	SetCurrentContextTokens int64
}

type sessionRepo struct {
	db *gorm.DB
}

// NewSessionRepo creates a new SessionRepo.
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
	now := time.Now()
	updates := map[string]any{
		"status":     string(status),
		"updated_at": now,
	}
	if status == model.SessionStatusClosed {
		updates["closed_at"] = now
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ?", id).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update session %s status: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update session %s status: %w", id, ErrSessionNotFound)
	}
	return nil
}

func (r *sessionRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	return updateWithAutoTimestamp(ctx, r.db, &model.Session{}, id, fields, "id = ?", "session", ErrSessionNotFound)
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
	// Copy to avoid mutating the caller's map.
	updates := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		updates[k] = v
	}
	updates["updated_at"] = time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ? AND status != ?", id, model.SessionStatusClosed).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update session %s fields: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("update session %s fields: %w", id, ErrSessionClosedOrNotFound)
	}
	return nil
}

func (r *sessionRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*model.Session, error) {
	return getByIDs[model.Session](ctx, r.db, ids, func(s *model.Session) uuid.UUID { return s.ID }, "sessions")
}

func (r *sessionRepo) ListByRoot(ctx context.Context, rootServerSessionID uuid.UUID, status string) ([]*model.Session, error) {
	var sessions []*model.Session
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("root_server_session_id = ? AND id != ? AND status = ?", rootServerSessionID, rootServerSessionID, status).
		Order("created_at ASC").
		Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("list sessions by root %s: %w", rootServerSessionID, err)
	}
	return sessions, nil
}

func (r *sessionRepo) FindActiveByParent(ctx context.Context, parentSessionID uuid.UUID) ([]*model.Session, error) {
	var sessions []*model.Session
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("parent_server_session_id = ? AND status != ?", parentSessionID, model.SessionStatusClosed).
		Order("created_at ASC").
		Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("find active sessions by parent %s: %w", parentSessionID, err)
	}
	return sessions, nil
}

func (r *sessionRepo) AtomicAddTokenUsage(ctx context.Context, sessionID uuid.UUID, delta TokenUsageDelta) error {
	updates := map[string]interface{}{
		"total_input_tokens":        gorm.Expr("total_input_tokens + ?", delta.InputDelta),
		"total_output_tokens":       gorm.Expr("total_output_tokens + ?", delta.OutputDelta),
		"total_tokens":              gorm.Expr("total_tokens + ?", delta.TotalDelta),
		"total_cached_read_tokens":  gorm.Expr("total_cached_read_tokens + ?", delta.CachedReadDelta),
		"total_cached_write_tokens": gorm.Expr("total_cached_write_tokens + ?", delta.CachedWriteDelta),
		"total_reasoning_tokens":    gorm.Expr("total_reasoning_tokens + ?", delta.ReasoningDelta),
		"total_cost_micros":         gorm.Expr("total_cost_micros + ?", delta.CostMicrosDelta),
		"last_token_update_at":      time.Now(),
	}
	if delta.SetEWMA > 0 {
		updates["token_estimate_ewma"] = delta.SetEWMA
	}
	if delta.SetCurrentContextTokens > 0 {
		updates["current_context_tokens"] = delta.SetCurrentContextTokens
	}
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ? AND status != ?", sessionID, model.SessionStatusClosed).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("atomic add token usage for session %s: %w", sessionID, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("session %s not found or closed: %w", sessionID, ErrSessionClosedOrNotFound)
	}
	return nil
}

func (r *sessionRepo) AtomicUpdateEWMA(ctx context.Context, sessionID uuid.UUID, ewma float64) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.Session{}).
		Where("id = ?", sessionID).
		Update("token_estimate_ewma", ewma)
	if result.Error != nil {
		return fmt.Errorf("atomic update ewma for session %s: %w", sessionID, result.Error)
	}
	return nil
}
