package repo

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/memory"
)

type memoryRepo struct {
	db *gorm.DB
}

// NewMemoryRepo 创建 memory.Repository 的 PostgreSQL 实现
func NewMemoryRepo(db *gorm.DB) memory.Repository {
	return &memoryRepo{db: db}
}

// ─── 创建 ───

func (r *memoryRepo) Create(ctx context.Context, m *memory.Memory) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(m).Error; err != nil {
		return fmt.Errorf("create memory: %w", err)
	}
	return nil
}

func (r *memoryRepo) BatchCreate(ctx context.Context, memories []*memory.Memory) error {
	if len(memories) == 0 {
		return nil
	}
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(&memories).Error; err != nil {
		return fmt.Errorf("batch create memories: %w", err)
	}
	return nil
}

// ─── 查询 ───

func (r *memoryRepo) GetByID(ctx context.Context, id uuid.UUID) (*memory.Memory, error) {
	var m memory.Memory
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&m, "id = ?", id).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, memory.ErrNotFound
		}
		return nil, fmt.Errorf("get memory %s: %w", id, err)
	}
	return &m, nil
}

func (r *memoryRepo) ListByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, opts memory.ListOptions) ([]*memory.Memory, error) {
	query := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("scope = ? AND scope_id = ?", scope, scopeID)

	// 应用过滤选项
	if len(opts.Types) > 0 {
		query = query.Where("type IN ?", opts.Types)
	} else if opts.Type != "" {
		query = query.Where("type = ?", opts.Type)
	}
	if len(opts.Tags) > 0 {
		query = applyTagsFilter(query, opts.Tags)
	}
	if !opts.CreatedAfter.IsZero() {
		query = query.Where("created_at >= ?", opts.CreatedAfter)
	}
	if !opts.CreatedBefore.IsZero() {
		query = query.Where("created_at <= ?", opts.CreatedBefore)
	}
	if opts.IncludeDeleted {
		query = query.Unscoped()
	}

	// 排序（白名单防止 SQL 注入）
	orderBy := opts.OrderBy
	if orderBy == "" {
		orderBy = "created_at DESC"
	} else {
		allowedOrders := map[string]bool{
			"created_at DESC": true,
			"created_at ASC":  true,
			"updated_at DESC": true,
			"updated_at ASC":  true,
			"title ASC":       true,
			"title DESC":      true,
		}
		if !allowedOrders[orderBy] {
			orderBy = "created_at DESC"
		}
	}
	query = query.Order(orderBy)

	// 分页
	if opts.Limit > 0 {
		query = query.Limit(opts.Limit)
	}
	if opts.Offset > 0 {
		query = query.Offset(opts.Offset)
	}

	var memories []*memory.Memory
	if err := query.Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("list memories by scope: %w", err)
	}
	return memories, nil
}

func (r *memoryRepo) ListRecentForInjection(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, maxCount, maxTokens int) ([]*memory.Memory, error) {
	if maxCount <= 0 {
		maxCount = 20
	}
	if maxTokens <= 0 {
		maxTokens = 12000
	}

	// 先查询最新的候选（多查一些用于 token 过滤）
	queryLimit := maxCount * 2
	if queryLimit > 100 {
		queryLimit = 100
	}

	var candidates []*memory.Memory
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("scope = ? AND scope_id = ?", scope, scopeID).
		Order("created_at DESC").
		Limit(queryLimit).
		Find(&candidates).Error
	if err != nil {
		return nil, fmt.Errorf("list recent memories for injection: %w", err)
	}

	// 按 token 预算过滤
	var result []*memory.Memory
	totalTokens := 0
	for _, m := range candidates {
		if len(result) >= maxCount {
			break
		}
		// 如果单条就超出预算且尚无结果，仍加入（避免返回空）
		if totalTokens+m.TokenCount > maxTokens && len(result) > 0 {
			break
		}
		result = append(result, m)
		totalTokens += m.TokenCount
	}

	return result, nil
}

func (r *memoryRepo) Search(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, query string, limit int) ([]*memory.Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	likeQuery := "%" + query + "%"

	var memories []*memory.Memory
	db := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("scope = ? AND scope_id = ?", scope, scopeID)

	// 根据数据库方言选择大小写敏感的模糊匹配
	switch getDialectName(db) {
	case "sqlite":
		// SQLite 的 LIKE 默认大小写不敏感（ASCII）
		db = db.Where("title LIKE ? OR content LIKE ? OR description LIKE ?",
			likeQuery, likeQuery, likeQuery)
	default:
		// PostgreSQL 使用 ILIKE 实现大小写不敏感匹配
		db = db.Where("title ILIKE ? OR content ILIKE ? OR description ILIKE ?",
			likeQuery, likeQuery, likeQuery)
	}

	err := db.Order("updated_at DESC").Limit(limit).Find(&memories).Error
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	return memories, nil
}

// ─── 更新 ───

func (r *memoryRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&memory.Memory{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return fmt.Errorf("update memory %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// ─── 删除（软删除）───

func (r *memoryRepo) Delete(ctx context.Context, id uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).Delete(&memory.Memory{}, "id = ?", id)
	if result.Error != nil {
		return fmt.Errorf("delete memory %s: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return memory.ErrNotFound
	}
	return nil
}

func (r *memoryRepo) DeleteByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("scope = ? AND scope_id = ?", scope, scopeID).
		Delete(&memory.Memory{}).Error; err != nil {
		return fmt.Errorf("delete memories by scope: %w", err)
	}
	return nil
}

// ─── 关系 ───

func (r *memoryRepo) GetLinked(ctx context.Context, id uuid.UUID, relation string) ([]*memory.Memory, error) {
	var memories []*memory.Memory
	query := DBFromContext(ctx, r.db).WithContext(ctx).
		Joins("JOIN memory_links ON memory_links.to_id = memories.id").
		Where("memory_links.from_id = ?", id)

	if relation != "" {
		query = query.Where("memory_links.relation = ?", relation)
	}

	if err := query.Find(&memories).Error; err != nil {
		return nil, fmt.Errorf("get linked memories: %w", err)
	}
	return memories, nil
}

func (r *memoryRepo) CreateLink(ctx context.Context, link *memory.MemoryLink) error {
	if err := DBFromContext(ctx, r.db).WithContext(ctx).Create(link).Error; err != nil {
		return fmt.Errorf("create memory link: %w", err)
	}
	return nil
}

func (r *memoryRepo) DeleteLink(ctx context.Context, fromID, toID uuid.UUID) error {
	result := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("from_id = ? AND to_id = ?", fromID, toID).
		Delete(&memory.MemoryLink{})
	if result.Error != nil {
		return fmt.Errorf("delete memory link: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// ─── 统计 ───

func (r *memoryRepo) CountTokensByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) (int, error) {
	var total int64
	err := DBFromContext(ctx, r.db).WithContext(ctx).
		Model(&memory.Memory{}).
		Where("scope = ? AND scope_id = ?", scope, scopeID).
		Select("COALESCE(SUM(token_count), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("count tokens by scope: %w", err)
	}
	return int(total), nil
}

// ─── 内部辅助 ───

// applyTagsFilter 根据数据库方言应用 Tags JSONB 过滤。
// PostgreSQL 使用 JSONB 包含操作符，SQLite 使用 LIKE 子串匹配。
func applyTagsFilter(query *gorm.DB, tags []string) *gorm.DB {
	dialect := getDialectName(query)
	if dialect == "postgres" || dialect == "postgresql" {
		// PostgreSQL: JSONB 包含操作符（任一标签匹配）
		return query.Where("tags ?| array(?)", tags)
	}
	// SQLite fallback: JSONB 存储为文本，使用 LIKE 子串匹配
	for _, tag := range tags {
		query = query.Where(`tags LIKE ?`, `%"`+tag+`"%`)
	}
	return query
}

// getDialectName 返回 GORM 数据库方言名称
func getDialectName(db *gorm.DB) string {
	if db == nil || db.Dialector == nil {
		return ""
	}
	return db.Name()
}
