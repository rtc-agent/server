package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/memory"
)

// setupMemoryTestDBWithUniqueConstraint creates a test DB with the unique constraint on memory_links.
func setupMemoryTestDBWithUniqueConstraint(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupMemoryTestDB(t)
	// Add unique constraint on (from_id, to_id) to match production behavior
	err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_links_from_to ON memory_links(from_id, to_id)").Error
	require.NoError(t, err)
	return db
}

// ─── P1-3: Duplicate Link Constraint Tests ───

func TestMemoryRepo_CreateLink_DuplicateReturnsErrDuplicateLink(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDBWithUniqueConstraint(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))

	// Create first link
	link1 := &memory.MemoryLink{FromID: mem1.ID, ToID: mem2.ID, Relation: "related"}
	require.NoError(t, repo.CreateLink(ctx, link1))

	// Attempt to create duplicate link (same from_id, to_id, different relation)
	link2 := &memory.MemoryLink{FromID: mem1.ID, ToID: mem2.ID, Relation: "depends_on"}
	err := repo.CreateLink(ctx, link2)
	require.Error(t, err, "duplicate link should return error")
	assert.True(t, errors.Is(err, memory.ErrDuplicateLink),
		"expected ErrDuplicateLink, got: %v", err)
}

func TestMemoryRepo_CreateLink_DifferentDirectionsAllowed(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDBWithUniqueConstraint(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))

	// Create link mem1 -> mem2
	link1 := &memory.MemoryLink{FromID: mem1.ID, ToID: mem2.ID, Relation: "related"}
	require.NoError(t, repo.CreateLink(ctx, link1))

	// Create reverse link mem2 -> mem1 (should be allowed)
	link2 := &memory.MemoryLink{FromID: mem2.ID, ToID: mem1.ID, Relation: "related"}
	require.NoError(t, repo.CreateLink(ctx, link2))

	// Verify both links exist
	linked1, err := repo.GetLinked(ctx, mem1.ID, "")
	require.NoError(t, err)
	assert.Len(t, linked1, 1)
	assert.Equal(t, mem2.ID, linked1[0].ID)

	linked2, err := repo.GetLinked(ctx, mem2.ID, "")
	require.NoError(t, err)
	assert.Len(t, linked2, 1)
	assert.Equal(t, mem1.ID, linked2[0].ID)
}

// ─── P2-1: Server-side Search Tests ───

func TestMemoryRepo_Search_EmptyQuery(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "Test Memory"
	require.NoError(t, repo.Create(ctx, mem))

	// Empty query should return no results (word-level matching requires at least one word)
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "", 10)
	require.NoError(t, err)
	assert.Len(t, results, 0)
}

func TestMemoryRepo_Search_SpecialCharacters(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "Test with % and _ characters"
	require.NoError(t, repo.Create(ctx, mem))

	// Search for literal % should work (escapeLikePattern should handle it)
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "%", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)

	// Search for literal _ should work
	results, err = repo.Search(ctx, memory.ScopeSession, sessionID, "_", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}

func TestMemoryRepo_Search_LimitZeroUsesDefault(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	for i := 0; i < 30; i++ {
		mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
		mem.Title = "Test"
		require.NoError(t, repo.Create(ctx, mem))
	}

	// Limit 0 should use default (20)
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "Test", 0)
	require.NoError(t, err)
	assert.Len(t, results, 20)

	// Negative limit should use default
	results, err = repo.Search(ctx, memory.ScopeSession, sessionID, "Test", -5)
	require.NoError(t, err)
	assert.Len(t, results, 20)
}

func TestMemoryRepo_Search_NoResults(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "Something"
	require.NoError(t, repo.Create(ctx, mem))

	// Search for non-existent term
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "nonexistent", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ─── P2-5: User Memory Scope Verification Tests ───

func TestMemoryRepo_UserScope_CannotAccessSessionScope(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	userID := uuid.New()

	// Create session-scoped memory
	sessionMem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	require.NoError(t, repo.Create(ctx, sessionMem))

	// Create user-scoped memory
	userMem := newTestMemoryRecord(t, memory.ScopeUser, userID, "feedback")
	require.NoError(t, repo.Create(ctx, userMem))

	// Verify session memory is not in user scope
	userResults, err := repo.ListByScope(ctx, memory.ScopeUser, userID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, userResults, 1)
	assert.Equal(t, userMem.ID, userResults[0].ID)

	// Verify user memory is not in session scope
	sessionResults, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, sessionResults, 1)
	assert.Equal(t, sessionMem.ID, sessionResults[0].ID)
}

func TestMemoryRepo_GlobalScope_Isolation(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	userID := uuid.New()

	// Create global memory
	globalMem := newTestMemoryRecord(t, memory.ScopeGlobal, uuid.Nil, "reference")
	require.NoError(t, repo.Create(ctx, globalMem))

	// Create user memory
	userMem := newTestMemoryRecord(t, memory.ScopeUser, userID, "feedback")
	require.NoError(t, repo.Create(ctx, userMem))

	// Global scope should only see global memories
	globalResults, err := repo.ListByScope(ctx, memory.ScopeGlobal, uuid.Nil, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, globalResults, 1)
	assert.Equal(t, globalMem.ID, globalResults[0].ID)

	// User scope should not see global memories
	userResults, err := repo.ListByScope(ctx, memory.ScopeUser, userID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, userResults, 1)
	assert.Equal(t, userMem.ID, userResults[0].ID)
}

// ─── P2-7: Search Sort Tiebreaker Tests ───

func TestMemoryRepo_Search_SortByUpdatedAt(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create memories with same title but different update times
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.Title = "Test"
	mem1.Content = "First"
	require.NoError(t, repo.Create(ctx, mem1))

	time.Sleep(10 * time.Millisecond)

	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2.Title = "Test"
	mem2.Content = "Second"
	require.NoError(t, repo.Create(ctx, mem2))

	time.Sleep(10 * time.Millisecond)

	mem3 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem3.Title = "Test"
	mem3.Content = "Third"
	require.NoError(t, repo.Create(ctx, mem3))

	// Search should return results ordered by updated_at DESC
	results, err := repo.Search(ctx, memory.ScopeSession, sessionID, "Test", 10)
	require.NoError(t, err)
	require.Len(t, results, 3)

	// Most recently created (and thus updated) should be first
	assert.Equal(t, "Third", results[0].Content)
	assert.Equal(t, "Second", results[1].Content)
	assert.Equal(t, "First", results[2].Content)
}

// ─── P1-1/P1-2: Validation Tests ───

func TestIsValidSessionMemoryType(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{"decision is valid", "decision", true},
		{"context is valid", "context", true},
		{"progress is valid", "progress", true},
		{"issue is valid", "issue", true},
		{"learnings is valid", "learnings", true},
		{"user is NOT valid for session", "user", false},
		{"feedback is NOT valid for session", "feedback", false},
		{"project is NOT valid for session", "project", false},
		{"reference is NOT valid for session", "reference", false},
		{"empty is invalid", "", false},
		{"random is invalid", "random", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, memory.IsValidSessionMemoryType(tt.typ))
		})
	}
}

func TestIsValidUserMemoryType(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{"user is valid", "user", true},
		{"feedback is valid", "feedback", true},
		{"project is valid", "project", true},
		{"reference is valid", "reference", true},
		{"decision is NOT valid for user", "decision", false},
		{"context is NOT valid for user", "context", false},
		{"progress is NOT valid for user", "progress", false},
		{"issue is NOT valid for user", "issue", false},
		{"learnings is NOT valid for user", "learnings", false},
		{"empty is invalid", "", false},
		{"random is invalid", "random", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, memory.IsValidUserMemoryType(tt.typ))
		})
	}
}

// ─── Edge Case: Token Count Edge Cases ───

func TestMemoryRepo_CountTokensByScope_NegativeTokenCount(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create memory with negative token count (edge case)
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.TokenCount = -10
	require.NoError(t, repo.Create(ctx, mem))

	// Should handle negative values gracefully
	total, err := repo.CountTokensByScope(ctx, memory.ScopeSession, sessionID)
	require.NoError(t, err)
	assert.Equal(t, -10, total)
}

func TestMemoryRepo_CountTokensByScope_MixedPositiveNegative(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.TokenCount = 100
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem2.TokenCount = -50
	mem3 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "progress")
	mem3.TokenCount = 200
	require.NoError(t, repo.Create(ctx, mem1))
	require.NoError(t, repo.Create(ctx, mem2))
	require.NoError(t, repo.Create(ctx, mem3))

	total, err := repo.CountTokensByScope(ctx, memory.ScopeSession, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 250, total) // 100 - 50 + 200
}

// ─── Edge Case: ListByScope with Invalid OrderBy ───

func TestMemoryRepo_ListByScope_InvalidOrderByFallsBackToDefault(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem1 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem1.Title = "First"
	require.NoError(t, repo.Create(ctx, mem1))

	time.Sleep(10 * time.Millisecond)

	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem2.Title = "Second"
	require.NoError(t, repo.Create(ctx, mem2))

	// Invalid OrderBy should fall back to default (created_at DESC)
	results, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{
		OrderBy: "invalid_column DESC",
	})
	require.NoError(t, err)
	assert.Len(t, results, 2)
	// Should be in default order (newest first)
	assert.Equal(t, "Second", results[0].Title)
	assert.Equal(t, "First", results[1].Title)
}

// ─── Edge Case: BatchCreate with Mixed Scopes ───

func TestMemoryRepo_BatchCreate_MixedScopes(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	userID := uuid.New()

	memories := []*memory.Memory{
		newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context"),
		newTestMemoryRecord(t, memory.ScopeUser, userID, "feedback"),
		newTestMemoryRecord(t, memory.ScopeGlobal, uuid.Nil, "reference"),
	}

	err := repo.BatchCreate(ctx, memories)
	require.NoError(t, err)

	// Verify all were created
	for _, mem := range memories {
		assert.NotEqual(t, uuid.Nil, mem.ID)
	}

	// Verify each scope has correct count
	sessionResults, err := repo.ListByScope(ctx, memory.ScopeSession, sessionID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, sessionResults, 1)

	userResults, err := repo.ListByScope(ctx, memory.ScopeUser, userID, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, userResults, 1)

	globalResults, err := repo.ListByScope(ctx, memory.ScopeGlobal, uuid.Nil, memory.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, globalResults, 1)
}

// ─── Edge Case: Delete and Recreate ───

func TestMemoryRepo_DeleteAndRecreate_SameID(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	require.NoError(t, repo.Create(ctx, mem))

	originalID := mem.ID

	// Delete
	require.NoError(t, repo.Delete(ctx, mem.ID))

	// Try to create new memory with same ID
	mem2 := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "decision")
	mem2.ID = originalID
	err := repo.Create(ctx, mem2)

	// Should succeed because soft-deleted record has different primary key constraint
	// (GORM soft delete sets deleted_at, doesn't remove the row)
	// This test documents the behavior
	if err != nil {
		// If it fails due to unique constraint on ID, that's expected
		// SQLite returns "UNIQUE constraint failed", PostgreSQL returns "duplicate key"
		assert.True(t, strings.Contains(err.Error(), "duplicate") ||
			strings.Contains(err.Error(), "UNIQUE constraint failed"),
			"expected duplicate/unique error, got: %v", err)
	} else {
		// If it succeeds, verify we can retrieve the new one
		got, err := repo.GetByID(ctx, originalID)
		require.NoError(t, err)
		assert.Equal(t, "decision", got.Type)
	}
}

// ─── Edge Case: Concurrent Updates ───

func TestMemoryRepo_Update_PartialFields(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	mem.Title = "Original Title"
	mem.Content = "Original Content"
	mem.Description = "Original Description"
	require.NoError(t, repo.Create(ctx, mem))

	// Update only title
	err := repo.Update(ctx, mem.ID, map[string]any{
		"title": "Updated Title",
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Title", got.Title)
	assert.Equal(t, "Original Content", got.Content)
	assert.Equal(t, "Original Description", got.Description)
}

func TestMemoryRepo_Update_EmptyFields(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestMemoryRecord(t, memory.ScopeSession, sessionID, "context")
	require.NoError(t, repo.Create(ctx, mem))

	// Update with empty fields map
	err := repo.Update(ctx, mem.ID, map[string]any{})
	require.NoError(t, err)

	// Should not error, just no-op
	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, mem.Title, got.Title)
}
