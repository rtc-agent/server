package repo

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

// setupSessionMemoryTestDB creates a SQLite in-memory database with SessionMemory table.
// Each call uses a unique DB name so parallel tests don't collide.
func setupSessionMemoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return setupTestDBWithModels(t, "smtest", &model.SessionMemory{})
}

func newTestSessionMemoryRepo(db *gorm.DB) SessionMemoryRepo {
	return NewSessionMemoryRepo(db)
}

func newTestSessionMemory(t *testing.T, sessionID uuid.UUID, category string) *model.SessionMemory {
	t.Helper()
	return &model.SessionMemory{
		SessionID: sessionID,
		Category:  category,
		Title:     "test memory",
		Content:   "test content",
		Metadata:  model.JSONObject{},
	}
}

func intPtr(v int) *int { return &v }

func TestSessionMemoryRepo_Create_And_GetByID(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestSessionMemory(t, sessionID, model.SessionMemoryCategoryDecision)

	err := repo.Create(ctx, mem)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, mem.ID, "BeforeCreate hook should have set ID")

	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, mem.ID, got.ID)
	assert.Equal(t, sessionID, got.SessionID)
	assert.Equal(t, model.SessionMemoryCategoryDecision, got.Category)
	assert.Equal(t, "test memory", got.Title)
	assert.Equal(t, "test content", got.Content)
}

func TestSessionMemoryRepo_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got %v", err)
}

func TestSessionMemoryRepo_ListBySession(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	// Create 3 memories for session A and 2 for session B.
	for i := 0; i < 3; i++ {
		mem := newTestSessionMemory(t, sessionA, model.SessionMemoryCategoryContext)
		mem.Title = "A"
		require.NoError(t, repo.Create(ctx, mem))
	}
	for i := 0; i < 2; i++ {
		mem := newTestSessionMemory(t, sessionB, model.SessionMemoryCategoryIssue)
		mem.Title = "B"
		require.NoError(t, repo.Create(ctx, mem))
	}

	// List session A memories.
	results, err := repo.ListBySession(ctx, sessionA, 10)
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, m := range results {
		assert.Equal(t, sessionA, m.SessionID)
		assert.Equal(t, "A", m.Title)
	}

	// Verify ordering: created_at DESC. The last created should come first.
	for i := 1; i < len(results); i++ {
		assert.True(t, !results[i].CreatedAt.After(results[i-1].CreatedAt),
			"expected DESC order: %v should be >= %v", results[i-1].CreatedAt, results[i].CreatedAt)
	}

	// List session B.
	resultsB, err := repo.ListBySession(ctx, sessionB, 10)
	require.NoError(t, err)
	assert.Len(t, resultsB, 2)
	for _, m := range resultsB {
		assert.Equal(t, sessionB, m.SessionID)
	}

	// Test limit.
	limited, err := repo.ListBySession(ctx, sessionA, 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)

	// Non-existent session.
	empty, err := repo.ListBySession(ctx, uuid.New(), 10)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestSessionMemoryRepo_SoftDelete_Single(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	mem := newTestSessionMemory(t, sessionID, model.SessionMemoryCategoryProgress)
	require.NoError(t, repo.Create(ctx, mem))

	// Verify it exists.
	got, err := repo.GetByID(ctx, mem.ID)
	require.NoError(t, err)
	assert.Equal(t, mem.ID, got.ID)

	// Delete.
	err = repo.Delete(ctx, mem.ID)
	require.NoError(t, err)

	// GetByID should return ErrNotFound.
	_, err = repo.GetByID(ctx, mem.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound after delete, got %v", err)

	// ListBySession should not include the deleted record.
	results, err := repo.ListBySession(ctx, sessionID, 10)
	require.NoError(t, err)
	assert.Empty(t, results)

	// Verify the record is physically removed from the table.
	var count int64
	db.Raw("SELECT COUNT(*) FROM session_memories WHERE id = ?", mem.ID).Scan(&count)
	assert.Equal(t, int64(0), count, "record should be physically deleted")
}

func TestSessionMemoryRepo_SoftDelete_BySession(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create 3 memories for the session.
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		mem := newTestSessionMemory(t, sessionID, model.SessionMemoryCategoryLearnings)
		require.NoError(t, repo.Create(ctx, mem))
		ids = append(ids, mem.ID)
	}

	// Delete all by session.
	err := repo.DeleteBySession(ctx, sessionID)
	require.NoError(t, err)

	// ListBySession should return empty.
	results, err := repo.ListBySession(ctx, sessionID, 10)
	require.NoError(t, err)
	assert.Empty(t, results)

	// Direct SQL: verify all 3 records are physically removed.
	var count int64
	db.Raw("SELECT COUNT(*) FROM session_memories WHERE session_id = ?", sessionID).Scan(&count)
	assert.Equal(t, int64(0), count, "all records should be physically deleted")

	// Verify none of them are visible via GetByID.
	for _, id := range ids {
		_, err := repo.GetByID(ctx, id)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrNotFound))
	}
}

func TestSessionMemoryRepo_ListRecentForInjection_TokenBudget(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create memories with known token counts (insert in order so created_at is predictable).
	memories := []*model.SessionMemory{
		{SessionID: sessionID, Category: "context", Title: "m1", Content: "c1", Metadata: model.JSONObject{}, TokenCount: intPtr(100)},
		{SessionID: sessionID, Category: "context", Title: "m2", Content: "c2", Metadata: model.JSONObject{}, TokenCount: intPtr(200)},
		{SessionID: sessionID, Category: "context", Title: "m3", Content: "c3", Metadata: model.JSONObject{}, TokenCount: intPtr(300)},
		{SessionID: sessionID, Category: "context", Title: "m4", Content: "c4", Metadata: model.JSONObject{}, TokenCount: intPtr(400)},
	}
	for _, mem := range memories {
		require.NoError(t, repo.Create(ctx, mem))
	}

	// Budget fits m4 + m3 + m2 (900) but not m1 (would be 1000).
	result, err := repo.ListRecentForInjection(ctx, sessionID, 10, 900)
	require.NoError(t, err)
	// m4 (400) + m3 (300) + m2 (200) = 900 fits exactly; m1 would push to 1000 > 900.
	assert.Len(t, result, 3)
	assert.Equal(t, "m4", result[0].Title)
	assert.Equal(t, "m3", result[1].Title)
	assert.Equal(t, "m2", result[2].Title)

	// maxCount limit: only 2 results.
	result2, err := repo.ListRecentForInjection(ctx, sessionID, 2, 10000)
	require.NoError(t, err)
	assert.Len(t, result2, 2)
	assert.Equal(t, "m4", result2[0].Title)
	assert.Equal(t, "m3", result2[1].Title)
}

func TestSessionMemoryRepo_ListRecentForInjection_SingleMemoryExceedsBudget(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Single memory that exceeds maxTokens.
	mem := &model.SessionMemory{
		SessionID:  sessionID,
		Category:   "context",
		Title:      "big",
		Content:    "huge content",
		Metadata:   model.JSONObject{},
		TokenCount: intPtr(5000),
	}
	require.NoError(t, repo.Create(ctx, mem))

	// Budget is 100, but the single memory should still be included to avoid empty result.
	result, err := repo.ListRecentForInjection(ctx, sessionID, 10, 100)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, "big", result[0].Title)
}

func TestSessionMemoryRepo_ListRecentForInjection_NilTokenCount(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Memory with nil TokenCount should be treated as 0.
	mem := &model.SessionMemory{
		SessionID:  sessionID,
		Category:   "context",
		Title:      "no-tokens",
		Content:    "content",
		Metadata:   model.JSONObject{},
		TokenCount: nil,
	}
	require.NoError(t, repo.Create(ctx, mem))

	// Even with very small budget, nil TokenCount (treated as 0) should be included.
	result, err := repo.ListRecentForInjection(ctx, sessionID, 10, 1)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, "no-tokens", result[0].Title)
}

func TestSessionMemoryRepo_CountTokensBySession(t *testing.T) {
	t.Parallel()
	db := setupSessionMemoryTestDB(t)
	repo := newTestSessionMemoryRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create memories with known token counts.
	memories := []*model.SessionMemory{
		{SessionID: sessionID, Category: "context", Title: "a", Content: "c", Metadata: model.JSONObject{}, TokenCount: intPtr(100)},
		{SessionID: sessionID, Category: "context", Title: "b", Content: "c", Metadata: model.JSONObject{}, TokenCount: intPtr(250)},
		{SessionID: sessionID, Category: "context", Title: "c", Content: "c", Metadata: model.JSONObject{}, TokenCount: intPtr(150)},
	}
	for _, mem := range memories {
		require.NoError(t, repo.Create(ctx, mem))
	}

	total, err := repo.CountTokensBySession(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 500, total)

	// Empty session.
	total, err = repo.CountTokensBySession(ctx, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 0, total)
}
