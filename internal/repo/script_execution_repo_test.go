package repo

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

var testDBCounter atomic.Int64

// setupTestDB creates a SQLite in-memory database with ScriptExecution table.
// Each call uses a unique DB name so parallel tests don't collide.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	n := testDBCounter.Add(1)
	dsn := fmt.Sprintf("file:test%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ScriptExecution{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func newTestScriptExecution(t *testing.T) *model.ScriptExecution {
	t.Helper()
	return &model.ScriptExecution{
		RtcID:     uuid.New(),
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		UserID:    uuid.New(),
		Title:     "test script",
		Action:    "eval",
		Status:    "success",
		Logs:      model.StringArray{"log1"},
		Warnings:  model.StringArray{},
		Errors:    model.StringArray{},
	}
}

func TestScriptExecutionRepo_Create(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := NewScriptExecutionRepo(db)
	ctx := context.Background()

	exec := newTestScriptExecution(t)
	if err := repo.Create(ctx, exec); err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if exec.ID == uuid.Nil {
		t.Error("Create() should have set ID via BeforeCreate hook")
	}
}

func TestScriptExecutionRepo_CreateDuplicateRtcID(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := NewScriptExecutionRepo(db)
	ctx := context.Background()

	exec1 := newTestScriptExecution(t)
	if err := repo.Create(ctx, exec1); err != nil {
		t.Fatalf("Create() first error: %v", err)
	}

	exec2 := newTestScriptExecution(t)
	exec2.RtcID = exec1.RtcID // same rtc_id
	exec2.ID = uuid.Nil

	err := repo.Create(ctx, exec2)
	if err == nil {
		t.Fatal("Create() should fail with duplicate rtc_id")
	}
}

func TestScriptExecutionRepo_GetByRtcID(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := NewScriptExecutionRepo(db)
	ctx := context.Background()

	exec := newTestScriptExecution(t)
	if err := repo.Create(ctx, exec); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	got, err := repo.GetByRtcID(ctx, exec.RtcID)
	if err != nil {
		t.Fatalf("GetByRtcID() error: %v", err)
	}
	if got.ID != exec.ID {
		t.Errorf("GetByRtcID() ID = %s, want %s", got.ID, exec.ID)
	}
	if got.Title != exec.Title {
		t.Errorf("GetByRtcID() Title = %q, want %q", got.Title, exec.Title)
	}
}

func TestScriptExecutionRepo_GetByRtcID_NotFound(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := NewScriptExecutionRepo(db)
	ctx := context.Background()

	_, err := repo.GetByRtcID(ctx, uuid.New())
	if !errors.Is(err, ErrScriptExecutionNotFound) {
		t.Errorf("GetByRtcID() error = %v, want ErrScriptExecutionNotFound", err)
	}
}

func TestScriptExecutionRepo_ListBySession(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := NewScriptExecutionRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	userID := uuid.New()

	// Create 3 executions for the same session.
	for i := 0; i < 3; i++ {
		exec := &model.ScriptExecution{
			RtcID:     uuid.New(),
			SessionID: sessionID,
			TurnID:    uuid.New(),
			UserID:    userID,
			Title:     "test",
			Action:    "eval",
			Status:    "success",
			Logs:      model.StringArray{},
			Warnings:  model.StringArray{},
			Errors:    model.StringArray{},
		}
		if err := repo.Create(ctx, exec); err != nil {
			t.Fatalf("Create() error: %v", err)
		}
	}

	// List all.
	execs, total, err := repo.ListBySession(ctx, sessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListBySession() error: %v", err)
	}
	if total != 3 {
		t.Errorf("ListBySession() total = %d, want 3", total)
	}
	if len(execs) != 3 {
		t.Errorf("ListBySession() len = %d, want 3", len(execs))
	}

	// Test pagination: limit 2.
	execs, total, err = repo.ListBySession(ctx, sessionID, 2, 0)
	if err != nil {
		t.Fatalf("ListBySession() error: %v", err)
	}
	if total != 3 {
		t.Errorf("ListBySession() total = %d, want 3", total)
	}
	if len(execs) != 2 {
		t.Errorf("ListBySession() len = %d, want 2", len(execs))
	}

	// Test pagination: offset 2.
	execs, total, err = repo.ListBySession(ctx, sessionID, 10, 2)
	if err != nil {
		t.Fatalf("ListBySession() error: %v", err)
	}
	if total != 3 {
		t.Errorf("ListBySession() total = %d, want 3", total)
	}
	if len(execs) != 1 {
		t.Errorf("ListBySession() len = %d, want 1", len(execs))
	}

	// Test empty session.
	execs, total, err = repo.ListBySession(ctx, uuid.New(), 10, 0)
	if err != nil {
		t.Fatalf("ListBySession() error: %v", err)
	}
	if total != 0 {
		t.Errorf("ListBySession() total = %d, want 0", total)
	}
	if len(execs) != 0 {
		t.Errorf("ListBySession() len = %d, want 0", len(execs))
	}
}

func TestScriptExecutionRepo_ListByUser(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := NewScriptExecutionRepo(db)
	ctx := context.Background()

	userID := uuid.New()

	// Create 2 executions for the same user.
	for i := 0; i < 2; i++ {
		exec := &model.ScriptExecution{
			RtcID:     uuid.New(),
			SessionID: uuid.New(),
			TurnID:    uuid.New(),
			UserID:    userID,
			Title:     "test",
			Action:    "run",
			Status:    "failed",
			Logs:      model.StringArray{},
			Warnings:  model.StringArray{},
			Errors:    model.StringArray{"error1"},
		}
		if err := repo.Create(ctx, exec); err != nil {
			t.Fatalf("Create() error: %v", err)
		}
	}

	execs, total, err := repo.ListByUser(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("ListByUser() error: %v", err)
	}
	if total != 2 {
		t.Errorf("ListByUser() total = %d, want 2", total)
	}
	if len(execs) != 2 {
		t.Errorf("ListByUser() len = %d, want 2", len(execs))
	}

	// Test empty user.
	execs, total, err = repo.ListByUser(ctx, uuid.New(), 10, 0)
	if err != nil {
		t.Fatalf("ListByUser() error: %v", err)
	}
	if total != 0 || len(execs) != 0 {
		t.Errorf("ListByUser() for non-existent user: total = %d, len = %d, want both 0", total, len(execs))
	}
}
