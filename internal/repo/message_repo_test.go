package repo

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// setupMessageTestDB creates a SQLite in-memory database with Message + Session tables.
func setupMessageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return setupTestDBWithModels(t, "msgtest", &model.Message{}, &model.Session{})
}

// createTestSession creates a minimal Session record (needed for FK constraints).
func createTestSession(t *testing.T, db *gorm.DB, id uuid.UUID) {
	t.Helper()
	session := &model.Session{
		ID:         id,
		OwnerKind:  "user",
		OwnerRefID: "test-user",
	}
	require.NoError(t, db.Create(session).Error)
}

// createMessagesWithOffsets creates N messages for a session with sequential global_offset starting from 1.
func createMessagesWithOffsets(t *testing.T, repo MessageRepo, ctx context.Context, sessionID uuid.UUID, count int) {
	t.Helper()
	messages := make([]*model.Message, count)
	for i := 0; i < count; i++ {
		messages[i] = &model.Message{
			SessionID:    sessionID,
			ClientID:     fmt.Sprintf("test-client-id-%d", i),
			GlobalOffset: uint32(i + 1),
			Role:         string(protocol.MessageRoleUser),
			Content:      "test message",
		}
	}
	require.NoError(t, repo.BatchCreate(ctx, messages))
}

func newTestMessageRepo(db *gorm.DB) MessageRepo {
	return NewMessageRepo(db)
}

// ─── ListRecentBySession Limit Tests ───

func TestMessageRepo_ListRecentBySession_MaxLimit(t *testing.T) {
	t.Parallel()
	db := setupMessageTestDB(t)
	repo := newTestMessageRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	createTestSession(t, db, sessionID)

	// Create 250 messages (exceeds maxLimit=200)
	createMessagesWithOffsets(t, repo, ctx, sessionID, 250)

	// Request limit=300 (should be capped to maxLimit=200)
	messages, err := repo.ListRecentBySession(ctx, sessionID, 300)
	require.NoError(t, err)
	assert.Equal(t, 200, len(messages), "should return at most maxLimit=200 messages")

	// Verify ordering: oldest first (ASC by global_offset)
	for i := 1; i < len(messages); i++ {
		assert.Less(t, messages[i-1].GlobalOffset, messages[i].GlobalOffset,
			"messages should be in ascending order by global_offset")
	}
}

func TestMessageRepo_ListRecentBySession_DefaultLimit(t *testing.T) {
	t.Parallel()
	db := setupMessageTestDB(t)
	repo := newTestMessageRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	createTestSession(t, db, sessionID)

	// Create 100 messages
	createMessagesWithOffsets(t, repo, ctx, sessionID, 100)

	// Request limit=0 (should use defaultLimit=50)
	messages, err := repo.ListRecentBySession(ctx, sessionID, 0)
	require.NoError(t, err)
	assert.Equal(t, 50, len(messages), "should return defaultLimit=50 messages when limit=0")

	// Verify ordering: oldest first (ASC by global_offset)
	for i := 1; i < len(messages); i++ {
		assert.Less(t, messages[i-1].GlobalOffset, messages[i].GlobalOffset,
			"messages should be in ascending order by global_offset")
	}
}

func TestMessageRepo_ListRecentBySession_NormalLimit(t *testing.T) {
	t.Parallel()
	db := setupMessageTestDB(t)
	repo := newTestMessageRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	createTestSession(t, db, sessionID)

	// Create 150 messages
	createMessagesWithOffsets(t, repo, ctx, sessionID, 150)

	// Request limit=100 (within normal range, should return exactly 100)
	messages, err := repo.ListRecentBySession(ctx, sessionID, 100)
	require.NoError(t, err)
	assert.Equal(t, 100, len(messages), "should return exactly limit=100 messages")

	// Verify ordering: oldest first (ASC by global_offset)
	for i := 1; i < len(messages); i++ {
		assert.Less(t, messages[i-1].GlobalOffset, messages[i].GlobalOffset,
			"messages should be in ascending order by global_offset")
	}

	// Verify we got the most recent 100 (offsets 51-150, not 1-100)
	assert.Equal(t, uint32(51), messages[0].GlobalOffset, "oldest message should have offset 51")
	assert.Equal(t, uint32(150), messages[99].GlobalOffset, "newest message should have offset 150")
}

func TestMessageRepo_ListRecentBySession_LessThanMax(t *testing.T) {
	t.Parallel()
	db := setupMessageTestDB(t)
	repo := newTestMessageRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	createTestSession(t, db, sessionID)

	// Create 30 messages (fewer than limit=100)
	createMessagesWithOffsets(t, repo, ctx, sessionID, 30)

	// Request limit=100 (should return all 30 available)
	messages, err := repo.ListRecentBySession(ctx, sessionID, 100)
	require.NoError(t, err)
	assert.Equal(t, 30, len(messages), "should return all 30 messages when fewer than limit")

	// Verify ordering: oldest first (ASC by global_offset)
	for i := 1; i < len(messages); i++ {
		assert.Less(t, messages[i-1].GlobalOffset, messages[i].GlobalOffset,
			"messages should be in ascending order by global_offset")
	}

	// Verify we got all messages (offsets 1-30)
	assert.Equal(t, uint32(1), messages[0].GlobalOffset, "oldest message should have offset 1")
	assert.Equal(t, uint32(30), messages[29].GlobalOffset, "newest message should have offset 30")
}

func TestMessageRepo_ListRecentBySession_NegativeLimit(t *testing.T) {
	t.Parallel()
	db := setupMessageTestDB(t)
	repo := newTestMessageRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	createTestSession(t, db, sessionID)

	// Create 100 messages
	createMessagesWithOffsets(t, repo, ctx, sessionID, 100)

	// Request limit=-1 (should use defaultLimit=50, same as limit=0)
	messages, err := repo.ListRecentBySession(ctx, sessionID, -1)
	require.NoError(t, err)
	assert.Equal(t, 50, len(messages), "negative limit should use defaultLimit=50")
}

func TestMessageRepo_ListRecentBySession_EmptySession(t *testing.T) {
	t.Parallel()
	db := setupMessageTestDB(t)
	repo := newTestMessageRepo(db)
	ctx := context.Background()

	sessionID := uuid.New()
	createTestSession(t, db, sessionID)

	// Request messages for session with no messages
	messages, err := repo.ListRecentBySession(ctx, sessionID, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, len(messages), "should return empty slice when no messages exist")
}
