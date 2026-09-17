package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/memory"
)

type memoryRepo struct {
	db *gorm.DB
}

// NewMemoryRepo creates a PostgreSQL-backed implementation of memory.Repository.
func NewMemoryRepo(db *gorm.DB) memory.Repository {
	return &memoryRepo{db: db}
}

// ─── Create ───

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

// ─── Query ───

func (r *memoryRepo) GetByID(ctx context.Context, id uuid.UUID) (*memory.Memory, error) {
	var m memory.Memory
	err := DBFromContext(ctx, r.db).WithContext(ctx).First(&m, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, memory.ErrNotFound
		}
		return nil, fmt.Errorf("get memory %s: %w", id, err)
	}
	return &m, nil
}

func (r *memoryRepo) ListByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, opts memory.ListOptions) ([]*memory.Memory, error) {
	query := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("scope = ? AND scope_id = ?", scope, scopeID)

	// Apply type filter.
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

	// Sort (whitelist to prevent SQL injection).
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

	// Pagination.
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

	// Fetch extra candidates for token-based filtering.
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

	// Filter by token budget.
	var result []*memory.Memory
	totalTokens := 0
	for _, m := range candidates {
		if len(result) >= maxCount {
			break
		}
		// If a single item exceeds the budget and we have no results yet,
		// include it to avoid returning an empty list.
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
	likeQuery := "%" + escapeLikePattern(query) + "%"

	var memories []*memory.Memory
	db := DBFromContext(ctx, r.db).WithContext(ctx).
		Where("scope = ? AND scope_id = ?", scope, scopeID)

	// Choose case-insensitive pattern matching based on database dialect.
	switch getDialectName(db) {
	case "sqlite":
		// SQLite LIKE is case-insensitive for ASCII by default.
		db = db.Where("title LIKE ? OR content LIKE ? OR description LIKE ?",
			likeQuery, likeQuery, likeQuery)
	default:
		// PostgreSQL uses ILIKE for case-insensitive matching.
		db = db.Where("title ILIKE ? OR content ILIKE ? OR description ILIKE ?",
			likeQuery, likeQuery, likeQuery)
	}

	err := db.Order("updated_at DESC").Limit(limit).Find(&memories).Error
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	return memories, nil
}

// ─── Update ───

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

// ─── Delete (soft delete) ───

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

// ─── Relations ───

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

// ─── Statistics ───

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

// ─── Internal helpers ───

// applyTagsFilter applies Tags JSONB filtering based on database dialect.
// PostgreSQL uses JSONB containment operator; SQLite falls back to LIKE substring match.
func applyTagsFilter(query *gorm.DB, tags []string) *gorm.DB {
	dialect := getDialectName(query)
	if dialect == "postgres" || dialect == "postgresql" {
		// PostgreSQL: JSONB containment operator (any tag matches).
		return query.Where("tags ?| array(?)", tags)
	}
	// SQLite fallback: JSONB stored as text, use LIKE substring match.
	for _, tag := range tags {
		query = query.Where(`tags LIKE ?`, `%"`+escapeLikePattern(tag)+`"%`)
	}
	return query
}

// getDialectName returns the GORM database dialect name.
func getDialectName(db *gorm.DB) string {
	if db == nil || db.Dialector == nil {
		return ""
	}
	return db.Name()
}
