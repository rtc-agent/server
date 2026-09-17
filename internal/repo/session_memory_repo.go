package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// SessionMemoryRepo provides session memory persistence operations.
type SessionMemoryRepo interface {
	// Create stores a new session memory record.
	Create(ctx context.Context, memory *model.SessionMemory) error

	// BatchCreate stores multiple session memories in a single INSERT.
	BatchCreate(ctx context.Context, memories []*model.SessionMemory) error

	// GetByID looks up a session memory by ID.
	GetByID(ctx context.Context, id uuid.UUID) (*model.SessionMemory, error)

	// ListBySession lists all memories for a session,
	// ordered by created_at DESC.
	ListBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.SessionMemory, error)

	// ListByCategory lists memories for a session filtered by category,
	// ordered by created_at DESC.
	ListByCategory(ctx context.Context, sessionID uuid.UUID, category string, limit int) ([]*model.SessionMemory, error)

	// ListRecentForInjection lists the most recent memories for injection.
	// Ordered by created_at DESC; cumulative token_count is tracked until
	// maxTokens or maxCount is reached.
	ListRecentForInjection(ctx context.Context, sessionID uuid.UUID, maxCount int, maxTokens int) ([]*model.SessionMemory, error)

	// Update modifies specific fields of a session memory.
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// Delete removes a session memory (hard delete — SessionMemory uses
	// *time.Time instead of gorm.DeletedAt).
	Delete(ctx context.Context, id uuid.UUID) error

	// DeleteBySession deletes all memories for a session (soft delete via deleted_at).
	DeleteBySession(ctx context.Context, sessionID uuid.UUID) error

	// CountTokensBySession returns the total token count for a session's memories.
	CountTokensBySession(ctx context.Context, sessionID uuid.UUID) (int, error)
}

type sessionMemoryRepo struct {
	db *gorm.DB
}

// NewSessionMemoryRepo creates a new SessionMemoryRepo.
func NewSessionMemoryRepo(db *gorm.DB) SessionMemoryRepo {
	return &sessionMemoryRepo{db: db}
}

func (r *sessionMemoryRepo) Create(ctx context.Context, memory *model.SessionMemory) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(memory).Error; err != nil {
		return fmt.Errorf("create session memory: %w", err)
	}
	return nil
}

func (r *sessionMemoryRepo) BatchCreate(ctx context.Context, memories []*model.SessionMemory) error {
	if len(memories) == 0 {
		return nil
	}
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(&memories).Error; err != nil {
		return fmt.Errorf("batch create session memories: %w", err)
	}
	return nil
}

func (r *sessionMemoryRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.SessionMemory, error) {
	var memory model.SessionMemory
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&memory, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("session memory %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get session memory %s: %w", id, err)
	}
	return &memory, nil
}

func (r *sessionMemoryRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.SessionMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	var memories []*model.SessionMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("created_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("list session memories by session %s: %w", sessionID, err)
	}
	return memories, nil
}

func (r *sessionMemoryRepo) ListByCategory(ctx context.Context, sessionID uuid.UUID, category string, limit int) ([]*model.SessionMemory, error) {
	return listByCategory[model.SessionMemory](ctx, r.db, "session_id", sessionID, category, limit, "created_at DESC", "", "session memories")
}

// ListRecentForInjection lists the most recent memories for injection.
// Strategy: fetch the newest memories (over-fetching), then accumulate
// token_count until maxTokens or maxCount is reached.
func (r *sessionMemoryRepo) ListRecentForInjection(ctx context.Context, sessionID uuid.UUID, maxCount int, maxTokens int) ([]*model.SessionMemory, error) {
	if maxCount <= 0 {
		maxCount = 20
	}
	if maxTokens <= 0 {
		maxTokens = 12000
	}

	// Fetch more memories than needed to allow for filtering.
	var allMemories []*model.SessionMemory
	queryLimit := maxCount * 2 // over-fetch to ensure enough candidates
	if queryLimit > 100 {
		queryLimit = 100
	}

	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("created_at DESC").
		Limit(queryLimit).
		Find(&allMemories).Error; err != nil {
		return nil, fmt.Errorf("list recent session memories for injection: %w", err)
	}

	// Accumulate token_count, selecting memories within budget.
	var result []*model.SessionMemory
	totalTokens := 0

	for _, mem := range allMemories {
		// Check count limit.
		if len(result) >= maxCount {
			break
		}

		// Get token_count (default to 0 if nil).
		tokenCount := 0
		if mem.TokenCount != nil {
			tokenCount = *mem.TokenCount
		}

		// Check token budget (if a single memory exceeds the limit, still
		// include it to avoid returning empty results).
		if totalTokens+tokenCount > maxTokens && len(result) > 0 {
			break
		}

		result = append(result, mem)
		totalTokens += tokenCount
	}

	return result, nil
}

func (r *sessionMemoryRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.SessionMemory{}).
		Where("id = ?", id).
		Updates(fields)
	if result.Error != nil {
		return fmt.Errorf("update session memory %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("session memory %s: %w", id, ErrNotFound)
	}
	return nil
}

func (r *sessionMemoryRepo) Delete(ctx context.Context, id uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("id = ?", id).
		Delete(&model.SessionMemory{})
	if result.Error != nil {
		return fmt.Errorf("delete session memory %s: %w", id, result.Error)
	}
	return nil
}

func (r *sessionMemoryRepo) DeleteBySession(ctx context.Context, sessionID uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ?", sessionID).
		Delete(&model.SessionMemory{}).Error; err != nil {
		return fmt.Errorf("delete session memories by session %s: %w", sessionID, err)
	}
	return nil
}

func (r *sessionMemoryRepo) CountTokensBySession(ctx context.Context, sessionID uuid.UUID) (int, error) {
	var totalTokens int
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.SessionMemory{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(SUM(token_count), 0)").
		Scan(&totalTokens).Error
	if err != nil {
		return 0, fmt.Errorf("count tokens by session %s: %w", sessionID, err)
	}
	return totalTokens, nil
}
