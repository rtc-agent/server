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

var loopTestDBCounter atomic.Int64

// setupLoopTestDB creates a SQLite in-memory database with Loop table.
func setupLoopTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	n := loopTestDBCounter.Add(1)
	dsn := fmt.Sprintf("file:looptest%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.Loop{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func newTestLoop(t *testing.T, sessionID uuid.UUID) *model.Loop {
	t.Helper()
	return &model.Loop{
		SessionID:       sessionID,
		Prompt:          "test prompt",
		IntervalSeconds: 60,
		MaxTurns:        10,
		CompletedTurns:  0,
		Status:          string(model.LoopStatusActive),
	}
}

// ─── Create Tests ───

func TestLoopRepo_Create(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)

	err := repo.Create(ctx, loop)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, loop.ID, "BeforeCreate hook should have set ID")

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, loop.ID, got.ID)
	assert.Equal(t, sessionID, got.SessionID)
	assert.Equal(t, "test prompt", got.Prompt)
	assert.Equal(t, 60, got.IntervalSeconds)
	assert.Equal(t, 10, got.MaxTurns)
}

// ─── GetByID Tests ───

func TestLoopRepo_GetByID(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, loop.ID, got.ID)
	assert.Equal(t, loop.SessionID, got.SessionID)
	assert.Equal(t, loop.Prompt, got.Prompt)
}

func TestLoopRepo_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	_, err := repo.GetByID(ctx, uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLoopNotFound), "expected ErrLoopNotFound, got %v", err)
}

// ─── FindActive Tests ───

func TestLoopRepo_FindActive_Found(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, loop.ID, got.ID)
	assert.Equal(t, string(model.LoopStatusActive), got.Status)
}

func TestLoopRepo_FindActive_None(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	got, err := repo.FindActive(ctx, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLoopRepo_FindActive_IgnoresTerminal(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Complete the loop
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"status": string(model.LoopStatusCompleted),
	}))

	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLoopRepo_FindActive_IgnoresPaused(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Pause the loop
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"status": string(model.LoopStatusPaused),
	}))

	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, got, "paused loop should not be returned by FindActive")
}

func TestLoopRepo_FindActive_ReturnsMostRecent(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	// Create first active loop
	loop1 := newTestLoop(t, sessionID)
	loop1.Prompt = "first"
	require.NoError(t, repo.Create(ctx, loop1))

	time.Sleep(10 * time.Millisecond)

	// Cancel first loop
	require.NoError(t, repo.Update(ctx, loop1.ID, map[string]any{
		"status": string(model.LoopStatusCancelled),
	}))

	// Create second active loop
	loop2 := newTestLoop(t, sessionID)
	loop2.Prompt = "second"
	require.NoError(t, repo.Create(ctx, loop2))

	got, err := repo.FindActive(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, loop2.ID, got.ID)
	assert.Equal(t, "second", got.Prompt)
}

func TestLoopRepo_FindActive_SessionIsolation(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	loop := newTestLoop(t, sessionA)
	require.NoError(t, repo.Create(ctx, loop))

	got, err := repo.FindActive(ctx, sessionB)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = repo.FindActive(ctx, sessionA)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, loop.ID, got.ID)
}

// ─── Update Tests ───

func TestLoopRepo_Update_BasicFields(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	err := repo.Update(ctx, loop.ID, map[string]any{
		"prompt":           "updated prompt",
		"interval_seconds": 120,
		"completed_turns":  3,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated prompt", got.Prompt)
	assert.Equal(t, 120, got.IntervalSeconds)
	assert.Equal(t, 3, got.CompletedTurns)
}

func TestLoopRepo_Update_TerminalStatus_AutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))
	assert.Nil(t, loop.CompletedAt)

	err := repo.Update(ctx, loop.ID, map[string]any{
		"status": string(model.LoopStatusCompleted),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.LoopStatusCompleted), got.Status)
	assert.NotNil(t, got.CompletedAt, "completed_at should be auto-filled for terminal status")
}

func TestLoopRepo_Update_TerminalStatus_ExplicitCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	explicitTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	err := repo.Update(ctx, loop.ID, map[string]any{
		"status":       string(model.LoopStatusCancelled),
		"completed_at": explicitTime,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.LoopStatusCancelled), got.Status)
	assert.NotNil(t, got.CompletedAt)
	assert.WithinDuration(t, explicitTime, *got.CompletedAt, time.Second)
}

func TestLoopRepo_Update_NonTerminalStatus_NoAutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	err := repo.Update(ctx, loop.ID, map[string]any{
		"completed_turns": 5,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Nil(t, got.CompletedAt, "completed_at should not be set for non-terminal updates")
}

func TestLoopRepo_Update_NotFound(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	err := repo.Update(ctx, uuid.New(), map[string]any{
		"prompt": "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLoopNotFound), "expected ErrLoopNotFound, got %v", err)
}

// ─── ListBySession Tests ───

func TestLoopRepo_ListBySession_Basic(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	for i := 0; i < 3; i++ {
		loop := newTestLoop(t, sessionA)
		loop.Prompt = fmt.Sprintf("A%d", i)
		require.NoError(t, repo.Create(ctx, loop))
	}
	for i := 0; i < 2; i++ {
		loop := newTestLoop(t, sessionB)
		loop.Prompt = fmt.Sprintf("B%d", i)
		require.NoError(t, repo.Create(ctx, loop))
	}

	results, err := repo.ListBySession(ctx, sessionA, nil, 0)
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, l := range results {
		assert.Equal(t, sessionA, l.SessionID)
	}

	resultsB, err := repo.ListBySession(ctx, sessionB, nil, 0)
	require.NoError(t, err)
	assert.Len(t, resultsB, 2)
}

func TestLoopRepo_ListBySession_SessionIsolation(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionA := uuid.New()
	sessionB := uuid.New()

	loop := newTestLoop(t, sessionA)
	require.NoError(t, repo.Create(ctx, loop))

	results, err := repo.ListBySession(ctx, sessionB, nil, 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestLoopRepo_ListBySession_Cursor(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	for i := 0; i < 5; i++ {
		loop := newTestLoop(t, sessionID)
		loop.Prompt = fmt.Sprintf("loop%d", i)
		require.NoError(t, repo.Create(ctx, loop))
	}

	all, err := repo.ListBySession(ctx, sessionID, nil, 50)
	require.NoError(t, err)
	assert.Len(t, all, 5)

	cursor := all[0].ID.String()
	results, err := repo.ListBySession(ctx, sessionID, &cursor, 50)
	require.NoError(t, err)
	assert.Len(t, results, 4)
	for _, r := range results {
		assert.NotEqual(t, all[0].ID, r.ID)
	}
}

func TestLoopRepo_ListBySession_DefaultLimit(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	for i := 0; i < 60; i++ {
		loop := newTestLoop(t, sessionID)
		require.NoError(t, repo.Create(ctx, loop))
	}

	results, err := repo.ListBySession(ctx, sessionID, nil, 0)
	require.NoError(t, err)
	assert.Len(t, results, 50)
}

func TestLoopRepo_ListBySession_ExplicitLimit(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()

	for i := 0; i < 10; i++ {
		loop := newTestLoop(t, sessionID)
		require.NoError(t, repo.Create(ctx, loop))
	}

	results, err := repo.ListBySession(ctx, sessionID, nil, 3)
	require.NoError(t, err)
	assert.Len(t, results, 3)
}

func TestLoopRepo_ListBySession_Empty(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	results, err := repo.ListBySession(ctx, uuid.New(), nil, 0)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ─── FindStaleLoops Tests ───

func TestLoopRepo_FindStaleLoops_FindsStale(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Set last_run_at to the past and clear asynq_task_id
	staleTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"last_run_at": staleTime,
	}))

	staleThreshold := time.Now().Add(-1 * time.Hour)
	results, err := repo.FindStaleLoops(ctx, staleThreshold)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, loop.ID, results[0].ID)
}

func TestLoopRepo_FindStaleLoops_ExcludesRecent(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Set last_run_at to recent (within threshold)
	recentTime := time.Now().Add(-10 * time.Minute)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"last_run_at": recentTime,
	}))

	staleThreshold := time.Now().Add(-1 * time.Hour)
	results, err := repo.FindStaleLoops(ctx, staleThreshold)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestLoopRepo_FindStaleLoops_ExcludesNonActive(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Pause the loop and set stale last_run_at
	staleTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"status":      string(model.LoopStatusPaused),
		"last_run_at": staleTime,
	}))

	staleThreshold := time.Now().Add(-1 * time.Hour)
	results, err := repo.FindStaleLoops(ctx, staleThreshold)
	require.NoError(t, err)
	assert.Empty(t, results, "paused loops should not be considered stale")
}

func TestLoopRepo_FindStaleLoops_ExcludesWithAsynqTaskID(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Set stale last_run_at but with asynq_task_id set
	staleTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"last_run_at":   staleTime,
		"asynq_task_id": "some-task-id",
	}))

	staleThreshold := time.Now().Add(-1 * time.Hour)
	results, err := repo.FindStaleLoops(ctx, staleThreshold)
	require.NoError(t, err)
	assert.Empty(t, results, "loops with asynq_task_id should not be stale")
}

func TestLoopRepo_FindStaleLoops_ExcludesNilLastRunAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Active loop with nil last_run_at — never run, should not be stale
	staleThreshold := time.Now().Add(-1 * time.Hour)
	results, err := repo.FindStaleLoops(ctx, staleThreshold)
	require.NoError(t, err)
	assert.Empty(t, results, "loops with nil last_run_at should not be stale")
}

// ─── FindExpiredLoops Tests ───

func TestLoopRepo_FindExpiredLoops_FindsExpired(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Set expires_at to the past
	expiredTime := time.Now().Add(-1 * time.Hour)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"expires_at": expiredTime,
	}))

	results, err := repo.FindExpiredLoops(ctx)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, loop.ID, results[0].ID)
}

func TestLoopRepo_FindExpiredLoops_ExcludesFuture(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Set expires_at to the future
	futureTime := time.Now().Add(1 * time.Hour)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"expires_at": futureTime,
	}))

	results, err := repo.FindExpiredLoops(ctx)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestLoopRepo_FindExpiredLoops_ExcludesNilExpiresAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// No expires_at set — should not be expired
	results, err := repo.FindExpiredLoops(ctx)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestLoopRepo_FindExpiredLoops_ExcludesNonActive(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	// Complete the loop with past expires_at
	expiredTime := time.Now().Add(-1 * time.Hour)
	require.NoError(t, repo.Update(ctx, loop.ID, map[string]any{
		"status":     string(model.LoopStatusCompleted),
		"expires_at": expiredTime,
	}))

	results, err := repo.FindExpiredLoops(ctx)
	require.NoError(t, err)
	assert.Empty(t, results, "completed loops should not be considered expired")
}

func TestLoopRepo_FindExpiredLoops_Empty(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	results, err := repo.FindExpiredLoops(ctx)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ─── Additional terminal status coverage ───

func TestLoopRepo_Update_CancelledStatus_AutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	err := repo.Update(ctx, loop.ID, map[string]any{
		"status": string(model.LoopStatusCancelled),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.LoopStatusCancelled), got.Status)
	assert.NotNil(t, got.CompletedAt)
}

func TestLoopRepo_Update_ExhaustedStatus_AutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	err := repo.Update(ctx, loop.ID, map[string]any{
		"status": string(model.LoopStatusExhausted),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.LoopStatusExhausted), got.Status)
	assert.NotNil(t, got.CompletedAt)
}

// ─── Paused status (non-terminal, should not trigger completed_at) ───

func TestLoopRepo_Update_PausedStatus_NoAutoCompletedAt(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	err := repo.Update(ctx, loop.ID, map[string]any{
		"status": string(model.LoopStatusPaused),
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	assert.Equal(t, string(model.LoopStatusPaused), got.Status)
	assert.Nil(t, got.CompletedAt, "completed_at should NOT be set for paused status")
}

// ─── Update with last_reason ───

func TestLoopRepo_Update_WithLastReason(t *testing.T) {
	t.Parallel()
	db := setupLoopTestDB(t)
	repo := NewLoopRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	loop := newTestLoop(t, sessionID)
	require.NoError(t, repo.Create(ctx, loop))

	reason := "loop exhausted after max turns"
	err := repo.Update(ctx, loop.ID, map[string]any{
		"status":      string(model.LoopStatusExhausted),
		"last_reason": reason,
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, loop.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastReason)
	assert.Equal(t, reason, *got.LastReason)
	assert.NotNil(t, got.CompletedAt)
}
