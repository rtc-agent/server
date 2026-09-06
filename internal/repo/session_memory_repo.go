package repo

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// SessionMemoryRepo 会话记忆仓储接口
type SessionMemoryRepo interface {
	// Create 创建一条会话记忆
	Create(ctx context.Context, memory *model.SessionMemory) error

	// BatchCreate 批量创建会话记忆
	BatchCreate(ctx context.Context, memories []*model.SessionMemory) error

	// GetByID 根据 ID 获取记忆
	GetByID(ctx context.Context, id uuid.UUID) (*model.SessionMemory, error)

	// ListBySession 列出会话的所有记忆
	// 按 created_at DESC 排序
	ListBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.SessionMemory, error)

	// ListByCategory 列出会话的指定分类记忆
	// 按 created_at DESC 排序
	ListByCategory(ctx context.Context, sessionID uuid.UUID, category string, limit int) ([]*model.SessionMemory, error)

	// ListRecentForInjection 列出用于注入的最新记忆
	// 按 created_at DESC 排序，累计 token_count 直到达到 maxTokens 或 maxCount
	ListRecentForInjection(ctx context.Context, sessionID uuid.UUID, maxCount int, maxTokens int) ([]*model.SessionMemory, error)

	// Update 更新记忆
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// Delete 软删除记忆
	Delete(ctx context.Context, id uuid.UUID) error

	// DeleteBySession 软删除会话的所有记忆
	DeleteBySession(ctx context.Context, sessionID uuid.UUID) error

	// CountTokensBySession 统计会话的总 token 数
	CountTokensBySession(ctx context.Context, sessionID uuid.UUID) (int, error)
}

type sessionMemoryRepo struct {
	db *gorm.DB
}

// NewSessionMemoryRepo 创建 SessionMemoryRepo
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
		if err == gorm.ErrRecordNotFound {
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
	if limit <= 0 {
		limit = 20
	}
	var memories []*model.SessionMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("session_id = ? AND category = ?", sessionID, category).
		Order("created_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("list session memories by category %s for session %s: %w", category, sessionID, err)
	}
	return memories, nil
}

// ListRecentForInjection 列出用于注入的最新记忆
// 策略：查询最新的记忆，累计 token_count 直到达到 maxTokens 或 maxCount
func (r *sessionMemoryRepo) ListRecentForInjection(ctx context.Context, sessionID uuid.UUID, maxCount int, maxTokens int) ([]*model.SessionMemory, error) {
	if maxCount <= 0 {
		maxCount = 20
	}
	if maxTokens <= 0 {
		maxTokens = 12000
	}

	// 先查询最新的记忆（多查一些，后续过滤）
	var allMemories []*model.SessionMemory
	queryLimit := maxCount * 2 // 多查一些，确保有足够的记忆
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

	// 累计 token_count，过滤出符合限制的记忆
	var result []*model.SessionMemory
	totalTokens := 0

	for _, mem := range allMemories {
		// 检查数量限制
		if len(result) >= maxCount {
			break
		}

		// 获取 token_count（如果为 nil，估算为 0）
		tokenCount := 0
		if mem.TokenCount != nil {
			tokenCount = *mem.TokenCount
		}

		// 检查 token 限制（如果单条记忆就超过限制，也加入，避免返回空）
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
