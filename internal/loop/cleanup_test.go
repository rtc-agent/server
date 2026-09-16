package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// mockGoalRepo implements repo.GoalRepo for testing.
type mockGoalRepo struct {
	mu      sync.Mutex
	goals   map[uuid.UUID]*model.Goal
	updated map[uuid.UUID]map[string]any
}

func newMockGoalRepo() *mockGoalRepo {
	return &mockGoalRepo{
		goals:   make(map[uuid.UUID]*model.Goal),
		updated: make(map[uuid.UUID]map[string]any),
	}
}

func (m *mockGoalRepo) Create(_ context.Context, goal *model.Goal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.goals[goal.ID] = goal
	return nil
}

func (m *mockGoalRepo) GetByID(_ context.Context, id uuid.UUID) (*model.Goal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.goals[id], nil
}

func (m *mockGoalRepo) FindActive(_ context.Context, sessionID uuid.UUID) (*model.Goal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.goals {
		if g.SessionID == sessionID && g.Status == model.GoalStatusActive {
			return g, nil
		}
	}
	return nil, nil
}

func (m *mockGoalRepo) Update(_ context.Context, id uuid.UUID, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated[id] = fields
	if g, ok := m.goals[id]; ok {
		if status, ok := fields["status"]; ok {
			switch v := status.(type) {
			case string:
				g.Status = model.GoalStatus(v)
			case model.GoalStatus:
				g.Status = v
			}
		}
	}
	return nil
}

func (m *mockGoalRepo) ListBySession(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Goal, error) {
	return nil, nil
}

// Ensure mockGoalRepo implements repo.GoalRepo.
var _ repo.GoalRepo = (*mockGoalRepo)(nil)

func TestCancelActiveGoal_NoActiveGoal(t *testing.T) {
	mockRepo := newMockGoalRepo()
	sessionID := uuid.Must(uuid.NewV7())

	// Should not panic or error when no active goal exists
	cancelActiveGoal(context.Background(), mockRepo, sessionID)

	if len(mockRepo.updated) != 0 {
		t.Errorf("expected no updates, got %d", len(mockRepo.updated))
	}
}

func TestCancelActiveGoal_WithActiveGoal(t *testing.T) {
	mockRepo := newMockGoalRepo()
	sessionID := uuid.Must(uuid.NewV7())
	goalID := uuid.Must(uuid.NewV7())

	goal := &model.Goal{
		ID:        goalID,
		SessionID: sessionID,
		Status:    model.GoalStatusActive,
		Condition: "test condition",
	}
	mockRepo.goals[goalID] = goal

	cancelActiveGoal(context.Background(), mockRepo, sessionID)

	// Verify the goal was updated to cancelled
	updates := mockRepo.updated[goalID]
	if updates == nil {
		t.Fatal("expected goal to be updated, but no update occurred")
	}
	if updates["status"] != model.GoalStatusCancelled {
		t.Errorf("status = %v, want %v", updates["status"], model.GoalStatusCancelled)
	}
	if updates["last_reason"] == nil {
		t.Error("expected last_reason to be set")
	}
}

func TestCancelActiveGoal_NilRepo(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV7())
	// Should not panic with nil repo
	cancelActiveGoal(context.Background(), nil, sessionID)
}

func TestCancelActiveGoal_OnlyCancelsActiveGoal(t *testing.T) {
	mockRepo := newMockGoalRepo()
	sessionID := uuid.Must(uuid.NewV7())
	goalID := uuid.Must(uuid.NewV7())

	// Create a completed goal (not active)
	goal := &model.Goal{
		ID:        goalID,
		SessionID: sessionID,
		Status:    model.GoalStatusCompleted,
		Condition: "test condition",
	}
	mockRepo.goals[goalID] = goal

	cancelActiveGoal(context.Background(), mockRepo, sessionID)

	// Should NOT update a completed goal
	if len(mockRepo.updated) != 0 {
		t.Errorf("expected no updates for completed goal, got %d", len(mockRepo.updated))
	}
}

// mockLoopRepo implements repo.LoopRepo for testing.
type mockLoopRepo struct {
	mu      sync.Mutex
	loops   map[uuid.UUID]*model.Loop
	updated map[uuid.UUID]map[string]any
}

func newMockLoopRepo() *mockLoopRepo {
	return &mockLoopRepo{
		loops:   make(map[uuid.UUID]*model.Loop),
		updated: make(map[uuid.UUID]map[string]any),
	}
}

func (m *mockLoopRepo) Create(_ context.Context, loop *model.Loop) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loops[loop.ID] = loop
	return nil
}

func (m *mockLoopRepo) GetByID(_ context.Context, id uuid.UUID) (*model.Loop, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loops[id], nil
}

func (m *mockLoopRepo) FindActive(_ context.Context, sessionID uuid.UUID) (*model.Loop, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.loops {
		if l.SessionID == sessionID && l.Status == model.LoopStatusActive {
			return l, nil
		}
	}
	return nil, nil
}

func (m *mockLoopRepo) Update(_ context.Context, id uuid.UUID, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated[id] = fields
	if l, ok := m.loops[id]; ok {
		if status, ok := fields["status"]; ok {
			switch v := status.(type) {
			case string:
				l.Status = model.LoopStatus(v)
			case model.LoopStatus:
				l.Status = v
			}
		}
	}
	return nil
}

func (m *mockLoopRepo) ListBySession(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Loop, error) {
	return nil, nil
}

func (m *mockLoopRepo) FindStaleLoops(_ context.Context, _ time.Time) ([]*model.Loop, error) {
	return nil, nil
}

func (m *mockLoopRepo) FindExpiredLoops(_ context.Context) ([]*model.Loop, error) {
	return nil, nil
}

// Ensure mockLoopRepo implements repo.LoopRepo.
var _ repo.LoopRepo = (*mockLoopRepo)(nil)

func TestCancelActiveLoop_NoActiveLoop(t *testing.T) {
	mockRepo := newMockLoopRepo()
	sessionID := uuid.Must(uuid.NewV7())

	// Should not panic when no active loop exists
	cancelActiveLoop(context.Background(), mockRepo, nil, sessionID)

	if len(mockRepo.updated) != 0 {
		t.Errorf("expected no updates, got %d", len(mockRepo.updated))
	}
}

func TestCancelActiveLoop_WithActiveLoop(t *testing.T) {
	mockRepo := newMockLoopRepo()
	sessionID := uuid.Must(uuid.NewV7())
	loopID := uuid.Must(uuid.NewV7())

	loop := &model.Loop{
		ID:        loopID,
		SessionID: sessionID,
		Status:    model.LoopStatusActive,
	}
	mockRepo.loops[loopID] = loop

	// nil inspector means asynq task cancellation is skipped
	cancelActiveLoop(context.Background(), mockRepo, nil, sessionID)

	// Verify the loop was updated to cancelled
	updates := mockRepo.updated[loopID]
	if updates == nil {
		t.Fatal("expected loop to be updated, but no update occurred")
	}
	if updates["status"] != model.LoopStatusCancelled {
		t.Errorf("status = %v, want %v", updates["status"], model.LoopStatusCancelled)
	}
}
