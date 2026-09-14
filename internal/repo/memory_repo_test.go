package repo

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/memory"
)

var memoryTestDBCounter atomic.Int64

// setupMemoryTestDB creates a SQLite in-memory database with Memory + MemoryLink tables.
func setupMemoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	n := memoryTestDBCounter.Add(1)
	dsn := fmt.Sprintf("file:memtest%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&memory.Memory{}, &memory.MemoryLink{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func newTestMemoryRecord(t *testing.T, scope memory.ScopeType, scopeID uuid.UUID, memType string) *memory.Memory {
	t.Helper()
	return &memory.Memory{
		Scope:      scope,
		ScopeID:    scopeID,
		Type:       memType,
		Title:      "test memory",
		Content:    "test content",
		Tags:       memory.StringArray{},
		Metadata:   memory.JSONBString("{}"),
		TokenCount: 10,
		Timestamp:  time.Now(),
	}
}

func newTestMemoryRepo(db *gorm.DB) memory.Repository {
	return NewMemoryRepo(db)
}

// ─── CRUD Tests ───

func TestMemoryRepo_Create_And_GetByID(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")

	err := repo.Create(ctx, mem)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, mem.ID, "BeforeCreate hook should have set ID")

	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, mem.ID, got.ID)
	assert.Equal(t, memory.ScopeSession, got.Scope)
	assert.Equal(t, sessionID, got.ScopeID)
	assert.Equal(t, "decision", got.Type)
	assert.Equal(t, "test memory", got.Title)
	assert.Equal(t, "test content", got.Content)
}

func TestMemoryRepo_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, memory.ErrNotFound), "expected memory.ErrNotFound, got %v", err)
}

func TestMemoryRepo_BatchCreate(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	memories := []*memory.Memory{
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context"),
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision"),
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress"),
	}

	err := repo.BatchCreate(ctx, memories)
	require.NoError(t, err)

	for _, mem := range memories {
		assert.NotEqual(t, uuid.Nil, mem.ID)
	}

	// Verify all were created
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestMemoryRepo_BatchCreate_Empty(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	err := repo.BatchCreate(ctx, nil)
	assert.NoError(t, err)

	err = repo.BatchCreate(ctx, []*memory.Memory{})
	assert.NoError(t, err)
}

func TestMemoryRepo_Update(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	require.NoError(t, repo.Create(ctx, mem))

	err := repo.Update(ctx, mem.ID, map[string]any{
		"title":   "updated title",
		"content": "updated content",
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated title", got.Title)
	assert.Equal(t, "updated content", got.Content)
}

func TestMemoryRepo_Update_NotFound(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	err := repo.Update(ctx, uuid.New(), map[string]any{"title": "test"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, memory.ErrNotFound))
}

// ─── ListByScope Tests ───

func TestMemoryRepo_ListByScope_Basic(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	for i := 0; i < 3; i++ {
		mem := newTestMemoryRecord(t, memory.ScopeSession, sessionA, "context")
		mem.Title = fmt.Sprintf("A%d", i)
		require.NoError(t, repo.Create(ctx, mem))
	}
	for i := 0; i < 2; i++ {
		mem := newTestMemoryRecord(t, memory.ScopeSession, sessionB, "issue")
		mem.Title = fmt.Sprintf("B%d", i)
		require.NoError(t, repo.Create(ctx, mem))
	}

	// List session A
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionA, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, m := range results {
		assert.Equal(t, sessionA, m.ScopeID)
	}

	// List session B
	resultsB, err := repo.ListByScope(ctx, memory.ScopeSession, sessionB, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, resultsB, 2)

	// Non-existent scope
	empty, err := repo.ListByScope(ctx, memory.ScopeSession, uuid.New(), memory.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestMemoryRepo_ListByScope_FilterByType(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")))
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")))
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")))

	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Type: "context"})
	require.NoError(t, err)
	assert.Len(t, results, 2)
	for _, m := range results {
		assert.Equal(t, "context", m.Type)
	}
}

func TestMemoryRepo_ListByScope_FilterByTypes(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")))
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")))
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress")))
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")))

	// Filter by multiple types: context + progress
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Types: []string{"context", "progress"}})
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, m := range results {
		assert.Contains(t, []string{"context", "progress"}, m.Type)
	}

	// Filter by single type using Types field
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Types: []string{"decision"}})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "decision", results[0].Type)

	// Empty Types slice should not filter
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Types: []string{}})
	require.NoError(t, err)
	assert.Len(t, results, 4)

	// Types takes precedence over Type
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{
		Type:  "decision",
		Types: []string{"context", "progress"},
	})
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, m := range results {
		assert.Contains(t, []string{"context", "progress"}, m.Type)
	}
}

func TestMemoryRepo_ListByScope_FilterByTags(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.Tags = memory.StringArray{"auth", "security"}
	require.NoError(t, repo.Create(ctx, mem1))

	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem2.Tags = memory.StringArray{"database", "performance"}
	require.NoError(t, repo.Create(ctx, mem2))

	mem3 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress")
	mem3.Tags = memory.StringArray{"auth", "testing"}
	require.NoError(t, repo.Create(ctx, mem3))

	// Filter by "auth" tag - should match mem1 and mem3
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Tags: []string{"auth"}})
	require.NoError(t, err)
	assert.Len(t, results, 2)

	// Filter by "database" tag - should match mem2 only
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Tags: []string{"database"}})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "decision", results[0].Type)
}

func TestMemoryRepo_ListByScope_Pagination(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")))
	}

	// Limit
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Limit: 3})
	require.NoError(t, err)
	assert.Len(t, results, 3)

	// Offset
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Offset: 3})
	require.NoError(t, err)
	assert.Len(t, results, 2)

	// Limit + Offset
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{Limit: 2, Offset: 1})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestMemoryRepo_ListByScope_TimeFilter(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create memories at different times
	before := time.Now().Add(-1 * time.Hour)
	for i := 0; i < 2; i++ {
		mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
		mem.Timestamp = before
		mem.CreatedAt = before
		require.NoError(t, repo.Create(ctx, mem))
	}

	middle := time.Now()
	after := time.Now().Add(1 * time.Hour)
	for i := 0; i < 3; i++ {
		mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
		if i == 0 {
			mem.Timestamp = middle
			mem.CreatedAt = middle
		} else {
			mem.Timestamp = after
			mem.CreatedAt = after
		}
		require.NoError(t, repo.Create(ctx, mem))
	}

	// Filter: created after middle (should get the 3 recent ones)
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{
		CreatedAfter: middle.Add(-1 * time.Second),
	})
	require.NoError(t, err)
	assert.Len(t, results, 3)

	// Filter: created before middle (should get the 2 old ones)
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{
		CreatedBefore: middle.Add(-1 * time.Millisecond),
	})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestMemoryRepo_ListByScope_IncludeDeleted(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))

	// Soft delete mem1
	require.NoError(t, repo.Delete(ctx, mem1.ID))

	// Without IncludeDeleted
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 1)

	// With IncludeDeleted
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{IncludeDeleted: true})
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestMemoryRepo_ListByScope_OrderBy(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.Title = "first"
	require.NoError(t, repo.Create(ctx, mem1))

	time.Sleep(10 * time.Millisecond) // ensure different timestamps

	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2.Title = "second"
	require.NoError(t, repo.Create(ctx, mem2))

	// Default order: created_at DESC
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, "second", results[0].Title)
	assert.Equal(t, "first", results[1].Title)

	// Ascending order
	results, err = repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{OrderBy: "created_at ASC"})
	require.NoError(t, err)
	assert.Equal(t, "first", results[0].Title)
	assert.Equal(t, "second", results[1].Title)
}

// ─── ListRecentForInjection Tests ───

func TestMemoryRepo_ListRecentForInjection_TokenBudget(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	memories := []*memory.Memory{
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context"),
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context"),
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context"),
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context"),
	}
	memories[0].TokenCount = 100
	memories[1].TokenCount = 200
	memories[2].TokenCount = 300
	memories[3].TokenCount = 400
	for _, mem := range memories {
		require.NoError(t, repo.Create(ctx, mem))
	}

	// Budget fits m4 + m3 + m2 (900) but not m1 (would be 1000)
	result, err := repo.ListRecentForInjection(ctx, memory.ScopeSession, sessionID, 10, 900)
	require.NoError(t, err)
	assert.Len(t, result, 3)
	// Most recent first (m4=400, m3=300, m2=200)
	assert.Equal(t, 400, result[0].TokenCount)
	assert.Equal(t, 300, result[1].TokenCount)
	assert.Equal(t, 200, result[2].TokenCount)

	// maxCount limit: only 2 results
	result2, err := repo.ListRecentForInjection(ctx, memory.ScopeSession, sessionID, 2, 10000)
	require.NoError(t, err)
	assert.Len(t, result2, 2)
}

func TestMemoryRepo_ListRecentForInjection_SingleMemoryExceedsBudget(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "big"
	mem.TokenCount = 5000
	require.NoError(t, repo.Create(ctx, mem))

	// Budget is 100, but the single memory should still be included
	result, err := repo.ListRecentForInjection(ctx, memory.ScopeSession, sessionID, 10, 100)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, "big", result[0].Title)
}

func TestMemoryRepo_ListRecentForInjection_ZeroTokenCount(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "zero-tokens"
	mem.TokenCount = 0
	require.NoError(t, repo.Create(ctx, mem))

	result, err := repo.ListRecentForInjection(ctx, memory.ScopeSession, sessionID, 10, 1)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, "zero-tokens", result[0].Title)
}

// ─── Search Tests ───

func TestMemoryRepo_Search(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.Title = "Authentication Flow"
	mem1.Content = "Implementing JWT authentication"
	require.NoError(t, repo.Create(ctx, mem1))

	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem2.Title = "Database Choice"
	mem2.Content = "Using PostgreSQL for JSONB support"
	require.NoError(t, repo.Create(ctx, mem2))

	mem3 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress")
	mem3.Title = "Auth Testing"
	mem3.Description = "Writing tests for auth module"
	require.NoError(t, repo.Create(ctx, mem3))

	// Search by title
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "auth", 10)
	require.NoError(t, err)
	assert.Len(t, results, 2) // "Authentication Flow" + "Auth Testing"

	// Search by content
	results, err = repo.Search(ctx, memory.ScopeSession, sessionID, "PostgreSQL", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "Database Choice", results[0].Title)

	// Search by description
	results, err = repo.Search(ctx, memory.ScopeSession, sessionID, "auth module", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)

	// Search with limit
	results, err = repo.Search(ctx, memory.ScopeSession, sessionID, "auth", 1)
	require.NoError(t, err)
	assert.Len(t, results, 1)

	// Search in different scope
	results, err = repo.Search(ctx, memory.ScopeSession, uuid.New(), "auth", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestMemoryRepo_Search_CaseInsensitive(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "PostgreSQL Setup"
	require.NoError(t, repo.Create(ctx, mem))

	// SQLite LIKE is case-insensitive for ASCII by default
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "postgresql", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)

	results, err = repo.Search(ctx, memory.ScopeSession, sessionID, "POSTGRESQL", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}

// ─── Delete Tests ───

func TestMemoryRepo_Delete_Single(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	require.NoError(t, repo.Create(ctx, mem))

	// Delete
	err := repo.Delete(ctx, mem.ID)
	require.NoError(t, err)

	// GetByID should return ErrNotFound
	_, err = repo.GetByID(ctx, mem.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, memory.ErrNotFound))

	// ListByScope should not include deleted record
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, results)

	// Verify soft delete: record still exists in DB with deleted_at set
	var count int64
	db.Raw("SELECT COUNT(*) FROM memories WHERE id = ?", mem.ID).Scan(&count)
	assert.Equal(t, int64(1), count, "record should still exist (soft delete)")
}

func TestMemoryRepo_Delete_NotFound(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	err := repo.Delete(ctx, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, memory.ErrNotFound))
}

func TestMemoryRepo_DeleteByScope(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	for i := 0; i < 3; i++ {
		require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")))
	}

	// Also create for a different scope
	otherSessionID := uuid.New()
	require.NoError(t, repo.Create(ctx, newTestMemoryRecord(t, memory.ScopeSession, otherSessionID, "context")))

	// Delete by scope
	err := repo.DeleteByScope(ctx, memory.ScopeSession, sessionID)
	require.NoError(t, err)

	// Original session should be empty
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, results)

	// Other session should be unaffected
	results, err = repo.ListByScope(ctx, memory.ScopeSession, otherSessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 1)

	// Soft deleted records should still exist in DB
	var count int64
	db.Raw("SELECT COUNT(*) FROM memories WHERE scope = ? AND scope_id = ?", sessionID.String(), sessionID.String()).Scan(&count)
	// Note: SQLite stores UUIDs as strings, so we need to match string representation
	// The above might not match; let's use the actual GORM model
	var total int64
	db.Model(&memory.Memory{}).Unscoped().
		Where("scope = ? AND scope_id = ?", memory.ScopeSession, sessionID).
		Count(&total)
	assert.Equal(t, int64(3), total, "soft-deleted records should still exist in DB")
}

// ─── MemoryLink Tests ───

func TestMemoryRepo_CreateLink_And_GetLinked(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem3 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress")
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))
	require.NoError(t, repo.Create(ctx, mem3))

	// Create links: mem1 -> mem2 (related), mem1 -> mem3 (depends_on)
	link1 := &memory.MemoryLink{FromID: mem1.ID, ToID: mem2.ID, Relation: "related"}
	link2 := &memory.MemoryLink{FromID: mem1.ID, ToID: mem3.ID, Relation: "depends_on"}
	require.NoError(t, repo.CreateLink(ctx, link1))
	require.NoError(t, repo.CreateLink(ctx, link2))
	assert.NotEqual(t, uuid.Nil, link1.ID)
	assert.NotEqual(t, uuid.Nil, link2.ID)

	// GetLinked: all linked from mem1
	linked, err := repo.GetLinked(ctx, mem1.ID, "")
	require.NoError(t, err)
	assert.Len(t, linked, 2)

	// GetLinked: only "related" from mem1
	linked, err = repo.GetLinked(ctx, mem1.ID, "related")
	require.NoError(t, err)
	assert.Len(t, linked, 1)
	assert.Equal(t, mem2.ID, linked[0].ID)

	// GetLinked: only "depends_on" from mem1
	linked, err = repo.GetLinked(ctx, mem1.ID, "depends_on")
	require.NoError(t, err)
	assert.Len(t, linked, 1)
	assert.Equal(t, mem3.ID, linked[0].ID)

	// GetLinked: mem2 has no outgoing links
	linked, err = repo.GetLinked(ctx, mem2.ID, "")
	require.NoError(t, err)
	assert.Empty(t, linked)
}

func TestMemoryRepo_DeleteLink(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))

	link := &memory.MemoryLink{FromID: mem1.ID, ToID: mem2.ID, Relation: "related"}
	require.NoError(t, repo.CreateLink(ctx, link))

	// Verify link exists
	linked, err := repo.GetLinked(ctx, mem1.ID, "")
	require.NoError(t, err)
	assert.Len(t, linked, 1)

	// Delete link
	err = repo.DeleteLink(ctx, mem1.ID, mem2.ID)
	require.NoError(t, err)

	// Verify link is removed
	linked, err = repo.GetLinked(ctx, mem1.ID, "")
	require.NoError(t, err)
	assert.Empty(t, linked)
}

// ─── CountTokensByScope Tests ───

func TestMemoryRepo_CountTokensByScope(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.TokenCount = 100
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem2.TokenCount = 250
	mem3 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress")
	mem3.TokenCount = 150
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))
	require.NoError(t, repo.Create(ctx, mem3))

	total, err := repo.CountTokensByScope(ctx, memory.ScopeSession, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 500, total)

	// Empty scope
	total, err = repo.CountTokensByScope(ctx, memory.ScopeSession, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 0, total)
}

func TestMemoryRepo_CountTokensByScope_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.TokenCount = 100
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem2.TokenCount = 200
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))

	// Delete mem1
	require.NoError(t, repo.Delete(ctx, mem1.ID))

	total, err := repo.CountTokensByScope(ctx, memory.ScopeSession, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 200, total, "soft-deleted records should not be counted")
}

// ─── User Scope Tests ───

func TestMemoryRepo_UserScope_CRUD(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	userID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeUser, userID, "feedback")
	mem.Title = "Prefer terse responses"
	mem.Content = "User prefers short, direct answers."
	mem.Tags = memory.StringArray{"preference", "communication"}
	mem.Description = "Communication style preference"

	require.NoError(t, repo.Create(ctx, mem))

	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeUser, got.Scope)
	assert.Equal(t, userID, got.ScopeID)
	assert.Equal(t, "feedback", got.Type)
	assert.Equal(t, "Prefer terse responses", got.Title)
	assert.Contains(t, []string(got.Tags), "preference")
	assert.Contains(t, []string(got.Tags), "communication")
}

func TestMemoryRepo_GlobalScope(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	mem := newTestMemoryRecord(t, memory.ScopeGlobal, uuid.Nil, "reference")
	mem.Title = "API Docs"
	require.NoError(t, repo.Create(ctx, mem))

	results, err := repo.ListByScope(ctx, memory.ScopeGlobal, uuid.Nil, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "API Docs", results[0].Title)
}
