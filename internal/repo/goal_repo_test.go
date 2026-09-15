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

	"github.com/rtc-agent/server/internal/model"
)

var goalTestDBCounter atomic.Int64

// setupGoalTestDB creates a SQLite in-memory database with Goal table.
func setupGoalTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	n := goalTestDBCounter.Add(1)
	dsn := fmt.Sprintf("file:goaltest%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.Goal{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func newTestGoal(t *testing.T, sessionID uuid.UUID) *model.Goal {
	t.Helper()
	return &model.Goal{
		SessionID:      sessionID,
		Condition:      "test condition",
		Status:         string(model.GoalStatusActive),
		CompletedTurns: 0,
		TokenUsage:     0,
		MaxTurns:       50,
	}
}

// ─── Create Tests ───

func TestGoalRepo_Create(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)

	err := repo.Create(ctx, goal)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, goal.ID, "BeforeCreate hook should have set ID")

	// Verify persisted
	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, goal.ID, got.ID)
	assert.Equal(t, sessionID, got.SessionID)
	assert.Equal(t, "test condition", got.Condition)
	assert.Equal(t, string(model.GoalStatusActive), got.Status)
	assert.Equal(t, 50, got.MaxTurns)
}

// ─── GetByID Tests ───

func TestGoalRepo_GetByID(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, goal.ID, got.ID)
	assert.Equal(t, goal.SessionID, got.SessionID)
	assert.Equal(t, goal.Condition, got.Condition)
}

func TestGoalRepo_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGoalNotFound), "expected ErrGoalNotFound, got %v", err)
}

// ─── FindActive Tests ───

func TestGoalRepo_FindActive_Found(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, goal.ID, got.ID)
	assert.Equal(t, string(model.GoalStatusActive), got.Status)
}

func TestGoalRepo_FindActive_None(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	// No goals at all
	got, err := repo.FindActive(ctx, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGoalRepo_FindActive_IgnoresTerminal(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create and complete a goal
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))
	require.NoError(t, repo.Update(ctx, goal.ID, map[string]any{
		"status": string(model.GoalStatusCompleted),
	}))

	// FindActive should return nil (no active goal)
	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGoalRepo_FindActive_ReturnsMostRecent(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create first active goal
	goal1 := newTestGoal(t, sessionID)
	goal1.Condition = "first"
	require.NoError(t, repo.Create(ctx, goal1))

	// Small delay to ensure different timestamps
	time.Sleep(10 * time.Millisecond)

	// Cancel first goal so we can create another active one
	require.NoError(t, repo.Update(ctx, goal1.ID, map[string]any{
		"status": string(model.GoalStatusCancelled),
	}))

	// Create second active goal
	goal2 := newTestGoal(t, sessionID)
	goal2.Condition = "second"
	require.NoError(t, repo.Create(ctx, goal2))

	// FindActive should return the most recent active goal
	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, goal2.ID, got.ID)
	assert.Equal(t, "second", got.Condition)
}

func TestGoalRepo_FindActive_SessionIsolation(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	goalA := newTestGoal(t, sessionA)
	require.NoError(t, repo.Create(ctx, goalA))

	// FindActive for sessionB should return nil
	got, err := repo.FindActive(ctx, sessionB)
	require.NoError(t, err)
	assert.Nil(t, got)

	// FindActive for sessionA should return goalA
	got, err = repo.FindActive(ctx, sessionA)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, goalA.ID, got.ID)
}

// ─── Update Tests ───

func TestGoalRepo_Update_BasicFields(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	err := repo.Update(ctx, goal.ID, map[string]any{
		"condition":       "updated condition",
		"completed_turns": 5,
		"token_usage":     1000,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated condition", got.Condition)
	assert.Equal(t, 5, got.CompletedTurns)
	assert.Equal(t, 1000, got.TokenUsage)
}

func TestGoalRepo_Update_TerminalStatus_AutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))
	assert.Nil(t, goal.CompletedAt)

	// Update to completed status without setting completed_at
	err := repo.Update(ctx, goal.ID, map[string]any{
		"status": string(model.GoalStatusCompleted),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.GoalStatusCompleted), got.Status)
	assert.NotNil(t, got.CompletedAt, "completed_at should be auto-filled for terminal status")
}

func TestGoalRepo_Update_TerminalStatus_ExplicitCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	explicitTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Update to completed status WITH explicit completed_at
	err := repo.Update(ctx, goal.ID, map[string]any{
		"status":       string(model.GoalStatusCompleted),
		"completed_at": explicitTime,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.GoalStatusCompleted), got.Status)
	assert.NotNil(t, got.CompletedAt)
	// The explicit time should be preserved (not overwritten by NOW())
	assert.WithinDuration(t, explicitTime, *got.CompletedAt, time.Second)
}

func TestGoalRepo_Update_NonTerminalStatus_NoAutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	// Update non-status field — completed_at should remain nil
	err := repo.Update(ctx, goal.ID, map[string]any{
		"completed_turns": 10,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Nil(t, got.CompletedAt, "completed_at should not be set for non-terminal updates")
}

func TestGoalRepo_Update_NotFound(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	err := repo.Update(ctx, uuid.New(), map[string]any{
		"condition": "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGoalNotFound), "expected ErrGoalNotFound, got %v", err)
}

// ─── ListBySession Tests ───

func TestGoalRepo_ListBySession_Basic(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	for i := 0; i < 3; i++ {
		goal := newTestGoal(t, sessionA)
		goal.Condition = fmt.Sprintf("A%d", i)
		require.NoError(t, repo.Create(ctx, goal))
	}
	for i := 0; i < 2; i++ {
		goal := newTestGoal(t, sessionB)
		goal.Condition = fmt.Sprintf("B%d", i)
		require.NoError(t, repo.Create(ctx, goal))
	}

	// List session A
	results, err := repo.ListBySession(ctx, sessionA, nil, 0)
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, g := range results {
		assert.Equal(t, sessionA, g.SessionID)
	}

	// List session B
	resultsB, err := repo.ListBySession(ctx, sessionB, nil, 0)
	require.NoError(t, err)
	assert.Len(t, resultsB, 2)
}

func TestGoalRepo_ListBySession_SessionIsolation(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	goal := newTestGoal(t, sessionA)
	require.NoError(t, repo.Create(ctx, goal))

	results, err := repo.ListBySession(ctx, sessionB, nil, 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestGoalRepo_ListBySession_Cursor(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	for i := 0; i < 5; i++ {
		goal := newTestGoal(t, sessionID)
		goal.Condition = fmt.Sprintf("goal%d", i)
		require.NoError(t, repo.Create(ctx, goal))
	}

	// Get first page (all 5)
	all, err := repo.ListBySession(ctx, sessionID, nil, 50)
	require.NoError(t, err)
	assert.Len(t, all, 5)

	// Use cursor from first result to get remaining
	// Since order is DESC by created_at, first result is the newest
	cursor := all[0].ID.String()
	results, err := repo.ListBySession(ctx, sessionID, &cursor, 50)
	require.NoError(t, err)
	assert.Len(t, results, 4)

	// Verify cursor excluded the first result
	for _, r := range results {
		assert.NotEqual(t, all[0].ID, r.ID)
	}
}

func TestGoalRepo_ListBySession_DefaultLimit(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create 60 goals (exceeds default limit of 50)
	for i := 0; i < 60; i++ {
		goal := newTestGoal(t, sessionID)
		require.NoError(t, repo.Create(ctx, goal))
	}

	// limit=0 should use default of 50
	results, err := repo.ListBySession(ctx, sessionID, nil, 0)
	require.NoError(t, err)
	assert.Len(t, results, 50)
}

func TestGoalRepo_ListBySession_ExplicitLimit(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	for i := 0; i < 10; i++ {
		goal := newTestGoal(t, sessionID)
		require.NoError(t, repo.Create(ctx, goal))
	}

	results, err := repo.ListBySession(ctx, sessionID, nil, 3)
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestGoalRepo_ListBySession_Empty(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	results, err := repo.ListBySession(ctx, uuid.New(), nil, 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ─── All terminal statuses ───

func TestGoalRepo_Update_CancelledStatus_AutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	err := repo.Update(ctx, goal.ID, map[string]any{
		"status": string(model.GoalStatusCancelled),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.GoalStatusCancelled), got.Status)
	assert.NotNil(t, got.CompletedAt)
}

func TestGoalRepo_Update_ExhaustedStatus_AutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	err := repo.Update(ctx, goal.ID, map[string]any{
		"status": string(model.GoalStatusExhausted),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.GoalStatusExhausted), got.Status)
	assert.NotNil(t, got.CompletedAt)
}

// ─── Update with status field but non-terminal ───

func TestGoalRepo_Update_ActiveStatus_NoAutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	// Explicitly set status to active (non-terminal)
	err := repo.Update(ctx, goal.ID, map[string]any{
		"status": string(model.GoalStatusActive),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.GoalStatusActive), got.Status)
	assert.Nil(t, got.CompletedAt, "completed_at should NOT be auto-filled for active status")
}

// ─── Update with last_reason ───

func TestGoalRepo_Update_WithLastReason(t *testing.T) {
	t.Parallel()
	db := setupGoalTestDB(t)
	repo := NewGoalRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	goal := newTestGoal(t, sessionID)
	require.NoError(t, repo.Create(ctx, goal))

	reason := "max turns reached"
	err := repo.Update(ctx, goal.ID, map[string]any{
		"status":      string(model.GoalStatusExhausted),
		"last_reason": reason,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, goal.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastReason)
	assert.Equal(t, reason, *got.LastReason)
	assert.NotNil(t, got.CompletedAt)
}
