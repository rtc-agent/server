package repo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
)

// setupMigrateTestDB creates a SQLite in-memory database with both old and new tables.
func setupMigrateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return setupTestDBWithModels(t, "migratetest",
		&model.SessionMemory{},
		&model.UserMemory{},
		&memory.Memory{},
		&memory.MemoryLink{},
	)
}

func TestMigrateToUnifiedMemory_SessionMemories(t *testing.T) {
	ctx := context.Background()
	db := setupMigrateTestDB(t)

	// Create old session memories
	sessionID := uuid.New()
	oldTokenCount := 100
	oldMem := &model.SessionMemory{
		ID:         uuid.New(),
		SessionID:  sessionID,
		Category:   "decision",
		Title:      "Test Decision",
		Content:    "We decided to use PostgreSQL",
		Metadata:   model.JSONB[any]{map[string]any{"key": "value"}},
		TokenCount: &oldTokenCount,
		CreatedAt:  time.Now().Add(-time.Hour),
		UpdatedAt:  time.Now(),
	}
	require.NoError(t, db.Create(oldMem).Error)

	// Run migration
	err := MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Verify new memory was created
	var newMem memory.Memory
	err = db.First(&newMem, "id = ?", oldMem.ID).Error
	require.NoError(t, err)

	assert.Equal(t, memory.ScopeSession, newMem.Scope)
	assert.Equal(t, sessionID, newMem.ScopeID)
	assert.Equal(t, "decision", newMem.Type)
	assert.Equal(t, "Test Decision", newMem.Title)
	assert.Equal(t, "We decided to use PostgreSQL", newMem.Content)
	assert.Equal(t, 100, newMem.TokenCount)
	assert.Equal(t, oldMem.CreatedAt.Unix(), newMem.Timestamp.Unix())
}

func TestMigrateToUnifiedMemory_UserMemories(t *testing.T) {
	ctx := context.Background()
	db := setupMigrateTestDB(t)

	// Create old user memory
	userID := uuid.New()
	sourceSessionID := uuid.New()
	desc := "User prefers dark mode"
	oldMem := &model.UserMemory{
		ID:              uuid.New(),
		UserID:          userID,
		Category:        "user",
		Importance:      "high",
		Title:           "UI Preference",
		Content:         "User prefers dark mode for all applications",
		Description:     &desc,
		Tags:            model.StringArray{"ui", "preference"},
		Metadata:        model.JSONB[any]{},
		SourceSessionID: &sourceSessionID,
		AccessCount:     5,
		CreatedAt:       time.Now().Add(-24 * time.Hour),
		UpdatedAt:       time.Now(),
	}
	require.NoError(t, db.Create(oldMem).Error)

	// Run migration
	err := MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Verify new memory was created
	var newMem memory.Memory
	err = db.First(&newMem, "id = ?", oldMem.ID).Error
	require.NoError(t, err)

	assert.Equal(t, memory.ScopeUser, newMem.Scope)
	assert.Equal(t, userID, newMem.ScopeID)
	assert.Equal(t, "user", newMem.Type)
	assert.Equal(t, "UI Preference", newMem.Title)
	assert.Equal(t, desc, newMem.Description)
	assert.Equal(t, "User prefers dark mode for all applications", newMem.Content)
	assert.Equal(t, memory.StringArray{"ui", "preference"}, newMem.Tags)
	assert.Greater(t, newMem.TokenCount, 0)
}

func TestMigrateToUnifiedMemory_MetadataConversion(t *testing.T) {
	ctx := context.Background()
	db := setupMigrateTestDB(t)

	// Create user memory with metadata fields
	userID := uuid.New()
	oldMem := &model.UserMemory{
		ID:          uuid.New(),
		UserID:      userID,
		Category:    "feedback",
		Importance:  "critical",
		Title:       "Critical Feedback",
		Content:     "This is very important feedback",
		Metadata:    model.JSONB[any]{},
		AccessCount: 10,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	require.NoError(t, db.Create(oldMem).Error)

	// Run migration
	err := MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Verify metadata was built
	var newMem memory.Memory
	err = db.First(&newMem, "id = ?", oldMem.ID).Error
	require.NoError(t, err)

	// Metadata should contain importance and access_count
	assert.Contains(t, string(newMem.Metadata), "critical")
	assert.Contains(t, string(newMem.Metadata), "10")
}

func TestMigrateToUnifiedMemory_SoftDeletedRecords(t *testing.T) {
	ctx := context.Background()
	db := setupMigrateTestDB(t)

	// Create soft-deleted session memory
	sessionID := uuid.New()
	deletedAt := time.Now().Add(-time.Hour)
	oldMem := &model.SessionMemory{
		ID:        uuid.New(),
		SessionID: sessionID,
		Category:  "context",
		Title:     "Deleted Context",
		Content:   "This memory was deleted",
		Metadata:  model.JSONB[any]{},
		DeletedAt: &deletedAt,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, db.Create(oldMem).Error)

	// Run migration
	err := MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Verify soft-deleted memory was migrated with DeletedAt set
	var newMem memory.Memory
	err = db.Unscoped().First(&newMem, "id = ?", oldMem.ID).Error
	require.NoError(t, err)

	assert.True(t, newMem.DeletedAt.Valid)
	assert.Equal(t, deletedAt.Unix(), newMem.DeletedAt.Time.Unix())
}

func TestMigrateToUnifiedMemory_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := setupMigrateTestDB(t)

	// Create old memories
	sessionID := uuid.New()
	oldMem := &model.SessionMemory{
		ID:        uuid.New(),
		SessionID: sessionID,
		Category:  "progress",
		Title:     "Progress Update",
		Content:   "We made good progress",
		Metadata:  model.JSONB[any]{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, db.Create(oldMem).Error)

	userID := uuid.New()
	oldUserMem := &model.UserMemory{
		ID:        uuid.New(),
		UserID:    userID,
		Category:  "project",
		Title:     "Project Info",
		Content:   "Project uses Go",
		Metadata:  model.JSONB[any]{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, db.Create(oldUserMem).Error)

	// Run migration first time
	err := MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Run migration second time (should be idempotent)
	err = MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Verify no duplicates were created
	var count int64
	db.Model(&memory.Memory{}).Count(&count)
	assert.Equal(t, int64(2), count)
}

func TestMigrateToUnifiedMemory_EmptyTables(t *testing.T) {
	ctx := context.Background()
	db := setupMigrateTestDB(t)

	// Run migration on empty tables (should succeed)
	err := MigrateToUnifiedMemory(ctx, db)
	require.NoError(t, err)

	// Verify no memories were created
	var count int64
	db.Model(&memory.Memory{}).Count(&count)
	assert.Equal(t, int64(0), count)
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected int
	}{
		{
			name:     "empty string",
			content:  "",
			expected: 0,
		},
		{
			name:     "pure ASCII",
			content:  "hello world this is a test",
			expected: 13, // 26 ASCII * 1 / 2 = 13
		},
		{
			name:     "pure Chinese",
			content:  "你好世界这是测试",
			expected: 8, // 8 runes * 2 / 2 = 8
		},
		{
			name:     "mixed content",
			content:  "hello 你好 world 世界",
			expected: 10, // (12 ASCII + 4 Chinese * 2) / 2 = 10
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := estimateTokens(tt.content)
			assert.Equal(t, tt.expected, result)
		})
	}
}
