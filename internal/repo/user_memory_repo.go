package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"gorm.io/gorm"
)

// UserMemoryRepo 用户记忆仓储接口
type UserMemoryRepo interface {
	// Create 创建一条用户记忆
	Create(ctx context.Context, memory *model.UserMemory) error

	// GetByID 根据 ID 获取记忆
	GetByID(ctx context.Context, id uuid.UUID) (*model.UserMemory, error)

	// ListByUser 列出用户的所有记忆
	ListByUser(ctx context.Context, userID uuid.UUID, limit int) ([]*model.UserMemory, error)

	// ListByCategory 列出用户的指定分类记忆
	ListByCategory(ctx context.Context, userID uuid.UUID, category string, limit int) ([]*model.UserMemory, error)

	// Update 更新记忆
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// Delete 软删除记忆
	Delete(ctx context.Context, id uuid.UUID) error

	// IncrementAccessCount 增加访问次数并更新最后访问时间
	IncrementAccessCount(ctx context.Context, id uuid.UUID) error

	// SearchByKeyword 关键词搜索（tags + title + content）
	SearchByKeyword(ctx context.Context, userID uuid.UUID, query string, limit int) ([]*model.UserMemory, error)

	// SearchByEmbedding 向量相似度搜索
	SearchByEmbedding(ctx context.Context, userID uuid.UUID, embedding []float32, limit int) ([]*model.UserMemory, error)

	// CountByUser 统计用户的记忆数量
	CountByUser(ctx context.Context, userID uuid.UUID) (int, error)
}

type userMemoryRepo struct {
	db *gorm.DB
}

// NewUserMemoryRepo 创建 UserMemoryRepo
func NewUserMemoryRepo(db *gorm.DB) UserMemoryRepo {
	return &userMemoryRepo{db: db}
}

func (r *userMemoryRepo) Create(ctx context.Context, memory *model.UserMemory) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(memory).Error; err != nil {
		return fmt.Errorf("create user memory: %w", err)
	}
	return nil
}

func (r *userMemoryRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.UserMemory, error) {
	var memory model.UserMemory
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&memory, "id = ?", id).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("user memory %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get user memory %s: %w", id, err)
	}
	return &memory, nil
}

func (r *userMemoryRepo) ListByUser(ctx context.Context, userID uuid.UUID, limit int) ([]*model.UserMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	var memories []*model.UserMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("updated_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("list user memories by user %s: %w", userID, err)
	}
	return memories, nil
}

func (r *userMemoryRepo) ListByCategory(ctx context.Context, userID uuid.UUID, category string, limit int) ([]*model.UserMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	var memories []*model.UserMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND category = ? AND deleted_at IS NULL", userID, category).
		Order("updated_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("list user memories by category %s for user %s: %w", category, userID, err)
	}
	return memories, nil
}

func (r *userMemoryRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	// 添加 updated_at
	updates := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		updates[k] = v
	}
	updates["updated_at"] = time.Now()

	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update user memory %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("user memory %s: %w", id, ErrNotFound)
	}
	return nil
}

func (r *userMemoryRepo) Delete(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Update("deleted_at", now)
	if result.Error != nil {
		return fmt.Errorf("delete user memory %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("user memory %s: %w", id, ErrNotFound)
	}
	return nil
}

func (r *userMemoryRepo) IncrementAccessCount(ctx context.Context, id uuid.UUID) error {
	now := time.Now()
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{
			"access_count":     gorm.Expr("access_count + 1"),
			"last_accessed_at": now,
		})
	if result.Error != nil {
		return fmt.Errorf("increment access count for user memory %s: %w", id, result.Error)
	}
	return nil
}

// SearchByKeyword 关键词搜索（tags + title + content）
// 使用 PostgreSQL 的 ILIKE 进行模糊匹配
func (r *userMemoryRepo) SearchByKeyword(ctx context.Context, userID uuid.UUID, query string, limit int) ([]*model.UserMemory, error) {
	if limit <= 0 {
		limit = 20
	}
	likeQuery := "%" + query + "%"

	var memories []*model.UserMemory
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Where("(title ILIKE ? OR content ILIKE ? OR description ILIKE ? OR tags::text ILIKE ?)",
			likeQuery, likeQuery, likeQuery, likeQuery).
		Order("updated_at DESC").
		Limit(limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("search user memories by keyword: %w", err)
	}
	return memories, nil
}

// SearchByEmbedding 向量相似度搜索
// 使用 pgvector 的余弦距离进行检索
// 注意：需要 pgvector 扩展支持
func (r *userMemoryRepo) SearchByEmbedding(ctx context.Context, userID uuid.UUID, embedding []float32, limit int) ([]*model.UserMemory, error) {
	if limit <= 0 {
		limit = 20
	}

	// 将 embedding 转换为 pgvector 格式字符串
	embeddingStr := vectorToString(embedding)

	var memories []*model.UserMemory
	// 使用余弦距离排序（<=> 是 pgvector 的余弦距离操作符）
	// 注意：这里使用原生 SQL 因为 GORM 不直接支持 pgvector 操作符
	query := `
		SELECT *, (embedding <=> ?) as distance
		FROM user_memories
		WHERE user_id = ? AND deleted_at IS NULL AND embedding IS NOT NULL
		ORDER BY embedding <=> ?
		LIMIT ?
	`

	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Raw(query, embeddingStr, userID, embeddingStr, limit).
		Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("search user memories by embedding: %w", err)
	}
	return memories, nil
}

func (r *userMemoryRepo) CountByUser(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int64
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&model.UserMemory{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("count user memories by user %s: %w", userID, err)
	}
	return int(count), nil
}

// vectorToString 将 float32 数组转换为 pgvector 格式字符串
// 例如: [1.0, 2.0, 3.0] -> "[1.0,2.0,3.0]"
func vectorToString(vec []float32) string {
	if len(vec) == 0 {
		return "[]"
	}
	result := "["
	for i, v := range vec {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf("%f", v)
	}
	result += "]"
	return result
}
