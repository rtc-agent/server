package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/pkg/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryRepo_Search_MultiWordOR(t *testing.T) {
	t.Parallel()
	db := setupMemoryTestDB(t)
	repo := newTestMemoryRepo(db)
	ctx := context.Background()

	userID := uuid.New()

	// Create test memories
	mem1 := newTestMemoryRecord(t, memory.ScopeUser, userID, "user")
	mem1.Title = "saveMemory tool test"
	mem1.Content = "Testing the saveMemory tool functionality"
	require.NoError(t, repo.Create(ctx, mem1))

	mem2 := newTestMemoryRecord(t, memory.ScopeUser, userID, "feedback")
	mem2.Title = "Code Style Preference"
	mem2.Content = "User prefers minimal comments in code"
	require.NoError(t, repo.Create(ctx, mem2))

	mem3 := newTestMemoryRecord(t, memory.ScopeUser, userID, "project")
	mem3.Title = "TaskManager Architecture"
	mem3.Content = "Uses IndexedDB for local storage"
	require.NoError(t, repo.Create(ctx, mem3))

	// Test 1: Multi-word search with OR logic
	// "test preference" should match mem1 (test) OR mem2 (preference)
	results, err := repo.Search(ctx, memory.ScopeUser, userID, "test preference", 10)
	require.NoError(t, err)
	assert.Len(t, results, 2, "Should match memories containing 'test' OR 'preference'")

	// Test 2: Single word search
	results, err = repo.Search(ctx, memory.ScopeUser, userID, "IndexedDB", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1, "Should match memory containing 'IndexedDB'")
	assert.Equal(t, "TaskManager Architecture", results[0].Title)

	// Test 3: Multi-word search where all words match different memories
	results, err = repo.Search(ctx, memory.ScopeUser, userID, "saveMemory IndexedDB", 10)
	require.NoError(t, err)
	assert.Len(t, results, 2, "Should match memories containing 'saveMemory' OR 'IndexedDB'")

	// Test 4: Search with no matches
	results, err = repo.Search(ctx, memory.ScopeUser, userID, "nonexistent xyz", 10)
	require.NoError(t, err)
	assert.Len(t, results, 0, "Should return empty for non-matching query")

	// Test 5: Verify scope isolation - search in different scope returns nothing
	otherUserID := uuid.New()
	results, err = repo.Search(ctx, memory.ScopeUser, otherUserID, "test", 10)
	require.NoError(t, err)
	assert.Len(t, results, 0, "Should not find memories from different user scope")

	// Test 6: Case-insensitive search
	results, err = repo.Search(ctx, memory.ScopeUser, userID, "TEST PREFERENCE", 10)
	require.NoError(t, err)
	assert.Len(t, results, 2, "Case-insensitive multi-word search should work")
}
