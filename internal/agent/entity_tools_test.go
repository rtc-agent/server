package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ─── Mock repos for testing ───

type mockGoalRepo struct {
	activeGoal *model.Goal
	activeErr  error
}

func (m *mockGoalRepo) Create(ctx context.Context, goal *model.Goal) error {
	return nil
}
func (m *mockGoalRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Goal, error) {
	return nil, nil
}
func (m *mockGoalRepo) FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Goal, error) {
	return m.activeGoal, m.activeErr
}
func (m *mockGoalRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	return nil
}
func (m *mockGoalRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Goal, error) {
	return nil, nil
}

type mockLoopRepo struct {
	activeLoop *model.Loop
	activeErr  error
}

func (m *mockLoopRepo) Create(ctx context.Context, loop *model.Loop) error {
	return nil
}
func (m *mockLoopRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Loop, error) {
	return nil, nil
}
func (m *mockLoopRepo) FindActive(ctx context.Context, sessionID uuid.UUID) (*model.Loop, error) {
	return m.activeLoop, m.activeErr
}
func (m *mockLoopRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	return nil
}
func (m *mockLoopRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, cursor *string, limit int) ([]*model.Loop, error) {
	return nil, nil
}
func (m *mockLoopRepo) FindStaleLoops(ctx context.Context, staleThreshold time.Time) ([]*model.Loop, error) {
	return nil, nil
}
func (m *mockLoopRepo) FindExpiredLoops(ctx context.Context) ([]*model.Loop, error) {
	return nil, nil
}

// ─── Mock TaskScheduler ───

type mockTaskScheduler struct {
	cancelErr error
	cancelled []string
}

func (m *mockTaskScheduler) ScheduleDelayed(ctx context.Context, taskType string, payload []byte, delay time.Duration, taskID string) (string, error) {
	if taskID != "" {
		return taskID, nil
	}
	return "mock-task-id", nil
}
func (m *mockTaskScheduler) Cancel(ctx context.Context, taskID string) error {
	m.cancelled = append(m.cancelled, taskID)
	return m.cancelErr
}

// ─── Mock Logger ───

type testEntityLogger struct {
	entries []map[string]any
}

func (l *testEntityLogger) Debug(ctx context.Context, msg string, fields map[string]any) {}
func (l *testEntityLogger) Info(ctx context.Context, msg string, fields map[string]any) {
	l.entries = append(l.entries, fields)
}
func (l *testEntityLogger) Warn(ctx context.Context, msg string, fields map[string]any) {
	l.entries = append(l.entries, fields)
}
func (l *testEntityLogger) Error(ctx context.Context, msg string, fields map[string]any) {}

// compile-time interface checks
var _ turnagent.Logger = (*testEntityLogger)(nil)

// ─── checkGoalLoopMutualExclusion Tests ───

func TestCheckGoalLoopMutualExclusion_CreateGoal_NoActiveLoop(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	deps := &usecase.Dependencies{
		LoopRepo: &mockLoopRepo{activeLoop: nil},
	}

	conflictMsg, err := checkGoalLoopMutualExclusion(context.Background(), deps, sessionID, "goal")
	require.NoError(t, err)
	assert.Empty(t, conflictMsg, "no conflict when no active loop exists")
}

func TestCheckGoalLoopMutualExclusion_CreateGoal_ActiveLoopExists(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	activeLoop := &model.Loop{
		ID:        uuid.New(),
		SessionID: sessionID,
		Status:    model.LoopStatusActive,
	}

	deps := &usecase.Dependencies{
		LoopRepo: &mockLoopRepo{activeLoop: activeLoop},
	}

	conflictMsg, err := checkGoalLoopMutualExclusion(context.Background(), deps, sessionID, "goal")
	require.NoError(t, err)
	assert.NotEmpty(t, conflictMsg, "conflict message expected when active loop exists")
	assert.Contains(t, conflictMsg, activeLoop.ID.String())
	assert.Contains(t, conflictMsg, "active loop")
}

func TestCheckGoalLoopMutualExclusion_CreateLoop_NoActiveGoal(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	deps := &usecase.Dependencies{
		GoalRepo: &mockGoalRepo{activeGoal: nil},
	}

	conflictMsg, err := checkGoalLoopMutualExclusion(context.Background(), deps, sessionID, "loop")
	require.NoError(t, err)
	assert.Empty(t, conflictMsg, "no conflict when no active goal exists")
}

func TestCheckGoalLoopMutualExclusion_CreateLoop_ActiveGoalExists(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	activeGoal := &model.Goal{
		ID:        uuid.New(),
		SessionID: sessionID,
		Status:    model.GoalStatusActive,
	}

	deps := &usecase.Dependencies{
		GoalRepo: &mockGoalRepo{activeGoal: activeGoal},
	}

	conflictMsg, err := checkGoalLoopMutualExclusion(context.Background(), deps, sessionID, "loop")
	require.NoError(t, err)
	assert.NotEmpty(t, conflictMsg)
	assert.Contains(t, conflictMsg, activeGoal.ID.String())
	assert.Contains(t, conflictMsg, "active goal")
}

func TestCheckGoalLoopMutualExclusion_UnknownEntityType(t *testing.T) {
	t.Parallel()

	deps := &usecase.Dependencies{}
	_, err := checkGoalLoopMutualExclusion(context.Background(), deps, uuid.New(), "unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown entity type")
}

func TestCheckGoalLoopMutualExclusion_NilLoopRepo_AllowsGoalCreation(t *testing.T) {
	t.Parallel()

	deps := &usecase.Dependencies{
		LoopRepo: nil, // nil means graceful degradation
	}

	conflictMsg, err := checkGoalLoopMutualExclusion(context.Background(), deps, uuid.New(), "goal")
	require.NoError(t, err)
	assert.Empty(t, conflictMsg, "nil LoopRepo should allow goal creation")
}

func TestCheckGoalLoopMutualExclusion_NilGoalRepo_AllowsLoopCreation(t *testing.T) {
	t.Parallel()

	deps := &usecase.Dependencies{
		GoalRepo: nil,
	}

	conflictMsg, err := checkGoalLoopMutualExclusion(context.Background(), deps, uuid.New(), "loop")
	require.NoError(t, err)
	assert.Empty(t, conflictMsg, "nil GoalRepo should allow loop creation")
}

func TestCheckGoalLoopMutualExclusion_FindActiveError_Propagated(t *testing.T) {
	t.Parallel()

	deps := &usecase.Dependencies{
		LoopRepo: &mockLoopRepo{activeErr: errors.New("db connection failed")},
	}

	_, err := checkGoalLoopMutualExclusion(context.Background(), deps, uuid.New(), "goal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "find active loop")
	assert.Contains(t, err.Error(), "db connection failed")
}

func TestCheckGoalLoopMutualExclusion_FindGoalError_Propagated(t *testing.T) {
	t.Parallel()

	deps := &usecase.Dependencies{
		GoalRepo: &mockGoalRepo{activeErr: errors.New("redis timeout")},
	}

	_, err := checkGoalLoopMutualExclusion(context.Background(), deps, uuid.New(), "loop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "find active goal")
	assert.Contains(t, err.Error(), "redis timeout")
}

// ─── cancelLoopAsynqTask Tests ───

func TestCancelLoopAsynqTask_Success_ClearsTaskID(t *testing.T) {
	t.Parallel()

	loop := &model.Loop{
		ID:          uuid.New(),
		AsynqTaskID: "task-123",
	}
	scheduler := &mockTaskScheduler{}
	logger := &testEntityLogger{}

	deps := &usecase.Dependencies{TaskScheduler: scheduler}
	updateFields := map[string]any{"status": model.LoopStatusCancelled}

	cancelLoopAsynqTask(context.Background(), deps, logger, loop, updateFields)

	assert.Equal(t, "", updateFields["asynq_task_id"], "asynq_task_id should be cleared")
	require.Len(t, scheduler.cancelled, 1)
	assert.Equal(t, "task-123", scheduler.cancelled[0])
}

func TestCancelLoopAsynqTask_NilScheduler_NoOp(t *testing.T) {
	t.Parallel()

	loop := &model.Loop{
		ID:          uuid.New(),
		AsynqTaskID: "task-123",
	}
	logger := &testEntityLogger{}

	deps := &usecase.Dependencies{TaskScheduler: nil}
	updateFields := map[string]any{"status": model.LoopStatusCancelled}

	cancelLoopAsynqTask(context.Background(), deps, logger, loop, updateFields)

	// Should not add asynq_task_id to fields when scheduler is nil
	_, exists := updateFields["asynq_task_id"]
	assert.False(t, exists, "asynq_task_id should not be set when scheduler is nil")
	assert.Empty(t, logger.entries, "no log entries expected when scheduler is nil")
}

func TestCancelLoopAsynqTask_EmptyTaskID_NoOp(t *testing.T) {
	t.Parallel()

	loop := &model.Loop{
		ID:          uuid.New(),
		AsynqTaskID: "", // no task ID
	}
	scheduler := &mockTaskScheduler{}
	logger := &testEntityLogger{}

	deps := &usecase.Dependencies{TaskScheduler: scheduler}
	updateFields := map[string]any{"status": model.LoopStatusCancelled}

	cancelLoopAsynqTask(context.Background(), deps, logger, loop, updateFields)

	assert.Empty(t, scheduler.cancelled, "Cancel should not be called when task ID is empty")
	_, exists := updateFields["asynq_task_id"]
	assert.False(t, exists, "asynq_task_id should not be set when loop has no task ID")
}

func TestCancelLoopAsynqTask_CancelError_LoggedButNotReturned(t *testing.T) {
	t.Parallel()

	loop := &model.Loop{
		ID:          uuid.New(),
		AsynqTaskID: "task-456",
	}
	scheduler := &mockTaskScheduler{cancelErr: errors.New("task not found")}
	logger := &testEntityLogger{}

	deps := &usecase.Dependencies{TaskScheduler: scheduler}
	updateFields := map[string]any{"status": model.LoopStatusCancelled}

	// Should not panic, should not return error
	cancelLoopAsynqTask(context.Background(), deps, logger, loop, updateFields)

	// asynq_task_id should still be cleared (status update proceeds regardless)
	assert.Equal(t, "", updateFields["asynq_task_id"])

	// Error should be logged
	require.Len(t, logger.entries, 1, "error should be logged")
	assert.Contains(t, logger.entries[0], "error")
	assert.Contains(t, logger.entries[0]["error"].(string), "task not found")
}

func TestCancelLoopAsynqTask_NilScheduler_NilLogger_NoPanic(t *testing.T) {
	t.Parallel()

	loop := &model.Loop{
		ID:          uuid.New(),
		AsynqTaskID: "task-789",
	}

	deps := &usecase.Dependencies{TaskScheduler: nil}
	updateFields := map[string]any{"status": model.LoopStatusCancelled}

	// Should not panic even with nil logger
	assert.NotPanics(t, func() {
		cancelLoopAsynqTask(context.Background(), deps, nil, loop, updateFields)
	})
}

func TestCancelLoopAsynqTask_PreservesExistingFields(t *testing.T) {
	t.Parallel()

	loop := &model.Loop{
		ID:          uuid.New(),
		AsynqTaskID: "task-abc",
	}
	scheduler := &mockTaskScheduler{}
	logger := &testEntityLogger{}

	deps := &usecase.Dependencies{TaskScheduler: scheduler}
	updateFields := map[string]any{
		"status":          model.LoopStatusCompleted,
		"completed_turns": 10,
		"last_reason":     "done",
	}

	cancelLoopAsynqTask(context.Background(), deps, logger, loop, updateFields)

	// Existing fields should be preserved
	assert.Equal(t, model.LoopStatusCompleted, updateFields["status"])
	assert.Equal(t, 10, updateFields["completed_turns"])
	assert.Equal(t, "done", updateFields["last_reason"])
	// And asynq_task_id should be added
	assert.Equal(t, "", updateFields["asynq_task_id"])
}
