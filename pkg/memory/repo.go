package memory

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repository defines the storage interface for Memory.
type Repository interface {
	// Create
	Create(ctx context.Context, m *Memory) error
	BatchCreate(ctx context.Context, memories []*Memory) error

	// Query
	GetByID(ctx context.Context, id uuid.UUID) (*Memory, error)
	ListByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID, opts ListOptions) ([]*Memory, error)
	ListRecentForInjection(ctx context.Context, scope ScopeType, scopeID uuid.UUID, maxCount, maxTokens int) ([]*Memory, error)
	Search(ctx context.Context, scope ScopeType, scopeID uuid.UUID, query string, limit int) ([]*Memory, error)

	// Update
	Update(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// Delete (soft delete)
	Delete(ctx context.Context, id uuid.UUID) error
	DeleteByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID) error

	// Relations
	GetLinked(ctx context.Context, id uuid.UUID, relation string) ([]*Memory, error)
	CreateLink(ctx context.Context, link *MemoryLink) error
	DeleteLink(ctx context.Context, fromID, toID uuid.UUID) error

	// Statistics
	CountTokensByScope(ctx context.Context, scope ScopeType, scopeID uuid.UUID) (int, error)
}

// ListOptions holds query options for listing memories.
type ListOptions struct {
	Type           string    // filter by type (backward compatible)
	Types          []string  // filter by multiple types (new)
	Tags           []string  // filter by tags (OR logic)
	CreatedAfter   time.Time // lower bound on creation time
	CreatedBefore  time.Time // upper bound on creation time
	IncludeDeleted bool      // whether to include soft-deleted records
	Limit          int       // page size
	Offset         int       // row offset
	OrderBy        string    // sort field (default: created_at DESC)
}
