package memory

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repository 定义 Memory 的存储接口
type Repository interface {
	// 创建
	Create(ctx context.Context, m *Memory) error
	BatchCreate(ctx context.Context, memories []*Memory) error

	// 查询
	GetByID(ctx context.Context, id uuid.UUID) (*Memory, error)
	ListByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID, opts ListOptions) ([]*Memory, error)
	ListRecentForInjection(ctx context.Context, scope ScopeType, scopeID uuid.UUID, maxCount, maxTokens int) ([]*Memory, error)
	Search(ctx context.Context, scope ScopeType, scopeID uuid.UUID, query string, limit int) ([]*Memory, error)

	// 更新
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// 删除（软删除）
	Delete(ctx context.Context, id uuid.UUID) error
	DeleteByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID) error

	// 关系
	GetLinked(ctx context.Context, id uuid.UUID, relation string) ([]*Memory, error)
	CreateLink(ctx context.Context, link *MemoryLink) error
	DeleteLink(ctx context.Context, fromID, toID uuid.UUID) error

	// 统计
	CountTokensByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID) (int, error)
}

// ListOptions 查询选项
type ListOptions struct {
	Type           string    // 按类型过滤（向后兼容）
	Types          []string  // 按多个类型过滤（新增）
	Tags           []string  // 按标签过滤（OR 逻辑）
	ScopeID        uuid.UUID // 按 ScopeID 过滤（可选，覆盖方法参数）
	CreatedAfter   time.Time // 创建时间下限
	CreatedBefore  time.Time // 创建时间上限
	IncludeDeleted bool      // 是否包含已删除记录
	Limit          int       // 分页大小
	Offset         int       // 偏移量
	OrderBy        string    // 排序字段（默认 created_at DESC）
}
