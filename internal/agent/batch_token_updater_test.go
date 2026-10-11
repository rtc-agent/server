package agent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionRepoForBatch is a minimal mock for testing batch token updater.
type mockSessionRepoForBatch struct {
	repo.SessionRepo
	updateCalls []model.TokenUsageDelta
	sessionID   uuid.UUID
}

func (m *mockSessionRepoForBatch) AtomicAddTokenUsage(ctx context.Context, sessionID uuid.UUID, delta model.TokenUsageDelta) error {
	m.updateCalls = append(m.updateCalls, delta)
	return nil
}

// TestBatchTokenUpdater_AccumulateAndFlush verifies that token updates are
// accumulated and flushed correctly.
func TestBatchTokenUpdater_AccumulateAndFlush(t *testing.T) {
	sessionID := uuid.New()
	mockRepo := &mockSessionRepoForBatch{sessionID: sessionID}
	log := logger.NoopLogger{}

	// Create batch updater with flush count = 3 (flush after 3 updates)
	updater := NewBatchTokenUpdater(mockRepo, log, 0, 3)
	require.NotNil(t, updater)

	ctx := context.Background()

	// Accumulate 2 updates (should not flush yet)
	delta1 := model.TokenUsageDelta{
		InputDelta:  100,
		OutputDelta: 50,
		TotalDelta:  150,
	}
	delta2 := model.TokenUsageDelta{
		InputDelta:  200,
		OutputDelta: 100,
		TotalDelta:  300,
	}

	err := updater.Accumulate(ctx, sessionID, delta1)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 0, "should not flush after 1 update")

	err = updater.Accumulate(ctx, sessionID, delta2)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 0, "should not flush after 2 updates")

	// Third update should trigger flush
	delta3 := model.TokenUsageDelta{
		InputDelta:  300,
		OutputDelta: 150,
		TotalDelta:  450,
	}
	err = updater.Accumulate(ctx, sessionID, delta3)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 1, "should flush after 3 updates")

	// Verify the accumulated delta
	flushed := mockRepo.updateCalls[0]
	assert.Equal(t, int64(600), flushed.InputDelta, "100+200+300")
	assert.Equal(t, int64(300), flushed.OutputDelta, "50+100+150")
	assert.Equal(t, int64(900), flushed.TotalDelta, "150+300+450")
}

// TestBatchTokenUpdater_TimeBasedFlush verifies that updates are flushed
// after the configured time interval.
func TestBatchTokenUpdater_TimeBasedFlush(t *testing.T) {
	sessionID := uuid.New()
	mockRepo := &mockSessionRepoForBatch{sessionID: sessionID}
	log := logger.NoopLogger{}

	// Create batch updater with 100ms flush interval
	updater := NewBatchTokenUpdater(mockRepo, log, 100*time.Millisecond, 100)
	require.NotNil(t, updater)

	ctx := context.Background()

	// Accumulate 1 update
	delta := model.TokenUsageDelta{
		InputDelta:  100,
		OutputDelta: 50,
		TotalDelta:  150,
	}
	err := updater.Accumulate(ctx, sessionID, delta)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 0, "should not flush immediately")

	// Wait for time-based flush
	time.Sleep(200 * time.Millisecond)

	// Should have flushed by now
	assert.Len(t, mockRepo.updateCalls, 1, "should flush after interval")
}

// TestBatchTokenUpdater_FlushSession verifies that FlushSession immediately
// flushes pending updates for a specific session.
func TestBatchTokenUpdater_FlushSession(t *testing.T) {
	sessionID := uuid.New()
	mockRepo := &mockSessionRepoForBatch{sessionID: sessionID}
	log := logger.NoopLogger{}

	updater := NewBatchTokenUpdater(mockRepo, log, 0, 100)
	require.NotNil(t, updater)

	ctx := context.Background()

	// Accumulate some updates
	delta := model.TokenUsageDelta{
		InputDelta:  100,
		OutputDelta: 50,
		TotalDelta:  150,
	}
	err := updater.Accumulate(ctx, sessionID, delta)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 0, "should not flush immediately")

	// Manually flush
	err = updater.FlushSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 1, "should flush immediately")
}

// TestBatchTokenUpdater_Shutdown verifies that Shutdown flushes all pending updates.
func TestBatchTokenUpdater_Shutdown(t *testing.T) {
	sessionID1 := uuid.New()
	sessionID2 := uuid.New()
	mockRepo := &mockSessionRepoForBatch{}
	log := logger.NoopLogger{}

	updater := NewBatchTokenUpdater(mockRepo, log, 0, 100)
	require.NotNil(t, updater)

	ctx := context.Background()

	// Accumulate updates for two sessions
	delta1 := model.TokenUsageDelta{InputDelta: 100, TotalDelta: 100}
	delta2 := model.TokenUsageDelta{InputDelta: 200, TotalDelta: 200}

	_ = updater.Accumulate(ctx, sessionID1, delta1)
	_ = updater.Accumulate(ctx, sessionID2, delta2)
	assert.Len(t, mockRepo.updateCalls, 0, "should not flush immediately")

	// Shutdown should flush all pending updates
	err := updater.Shutdown(ctx)
	require.NoError(t, err)
	assert.Len(t, mockRepo.updateCalls, 2, "should flush both sessions on shutdown")
}

// TestBatchTokenUpdater_SetEWMA_TakesLatest verifies that when multiple updates
// are accumulated, the latest SetEWMA value (if > 0) is used.
func TestBatchTokenUpdater_SetEWMA_TakesLatest(t *testing.T) {
	sessionID := uuid.New()
	mockRepo := &mockSessionRepoForBatch{sessionID: sessionID}
	log := logger.NoopLogger{}

	updater := NewBatchTokenUpdater(mockRepo, log, 0, 3)
	require.NotNil(t, updater)

	ctx := context.Background()

	// First update with EWMA = 1000
	delta1 := model.TokenUsageDelta{
		InputDelta: 100,
		SetEWMA:    1000,
	}
	_ = updater.Accumulate(ctx, sessionID, delta1)

	// Second update with EWMA = 2000 (should override)
	delta2 := model.TokenUsageDelta{
		InputDelta: 200,
		SetEWMA:    2000,
	}
	_ = updater.Accumulate(ctx, sessionID, delta2)

	// Third update with EWMA = 0 (should not override)
	delta3 := model.TokenUsageDelta{
		InputDelta: 300,
		SetEWMA:    0, // zero means don't update
	}
	_ = updater.Accumulate(ctx, sessionID, delta3)

	// Should have flushed
	require.Len(t, mockRepo.updateCalls, 1)

	// Verify EWMA is 2000 (the latest non-zero value)
	assert.Equal(t, float64(2000), mockRepo.updateCalls[0].SetEWMA, "should use latest non-zero EWMA")
}

// TestBatchTokenUpdater_Stats verifies that Stats returns correct counts.
func TestBatchTokenUpdater_Stats(t *testing.T) {
	sessionID := uuid.New()
	mockRepo := &mockSessionRepoForBatch{sessionID: sessionID}
	log := logger.NoopLogger{}

	updater := NewBatchTokenUpdater(mockRepo, log, 0, 100)
	require.NotNil(t, updater)

	ctx := context.Background()

	// Initially no pending sessions
	stats := updater.Stats()
	assert.Equal(t, 0, stats["pending_sessions"])

	// Accumulate an update
	delta := model.TokenUsageDelta{InputDelta: 100}
	_ = updater.Accumulate(ctx, sessionID, delta)

	// Should have 1 pending session
	stats = updater.Stats()
	assert.Equal(t, 1, stats["pending_sessions"])

	// Flush
	_ = updater.FlushSession(ctx, sessionID)

	// Should have 0 pending sessions
	stats = updater.Stats()
	assert.Equal(t, 0, stats["pending_sessions"])
}
