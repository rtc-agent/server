package taskscheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibikenasynq "github.com/hibiken/asynq"
)

// TestScheduleDelayed_Idempotency verifies that scheduling the same task ID
// multiple times is idempotent (returns ErrTaskIDConflict on duplicates).
func TestScheduleDelayed_Idempotency(t *testing.T) {
	// Start mini Redis
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	defer mr.Close()

	// Create scheduler
	scheduler, err := NewTaskScheduler(mr.Addr())
	if err != nil {
		t.Fatalf("failed to create scheduler: %v", err)
	}
	impl := scheduler.(*Impl)
	defer impl.Close()

	ctx := context.Background()
	taskType := "test:task"
	payload := []byte(`{"test": "data"}`)
	delay := 1 * time.Minute
	taskID := "test-task-id-123"

	// First schedule: should succeed
	id1, err := scheduler.ScheduleDelayed(ctx, taskType, payload, delay, taskID)
	if err != nil {
		t.Fatalf("first schedule failed: %v", err)
	}
	if id1 != taskID {
		t.Errorf("first schedule returned task ID %q; want %q", id1, taskID)
	}

	// Second schedule with same task ID: should return ErrTaskIDConflict
	id2, err := scheduler.ScheduleDelayed(ctx, taskType, payload, delay, taskID)
	if err == nil {
		t.Errorf("second schedule succeeded; want ErrTaskIDConflict")
	}
	if !errors.Is(err, hibikenasynq.ErrTaskIDConflict) {
		t.Errorf("second schedule error = %v; want ErrTaskIDConflict", err)
	}
	if id2 != "" {
		t.Errorf("second schedule returned task ID %q; want empty", id2)
	}

	// Verify task exists in asynq
	info, err := impl.inspector.GetTaskInfo(LoopQueue, taskID)
	if err != nil {
		t.Fatalf("failed to get task info: %v", err)
	}
	if info == nil {
		t.Fatal("task not found in asynq")
	}
	if info.Type != taskType {
		t.Errorf("task type = %q; want %q", info.Type, taskType)
	}
	// Task state can be "scheduled" (delayed) or "pending" (ready to process)
	// For delayed tasks, it starts as "scheduled" and transitions to "pending" when delay expires
	if info.State != hibikenasynq.TaskStateScheduled && info.State != hibikenasynq.TaskStatePending {
		t.Errorf("task state = %q; want scheduled or pending", info.State)
	}
}

// TestScheduleDelayed_EmptyTaskID verifies that passing empty task ID
// generates a unique ID (backward compatible behavior).
func TestScheduleDelayed_EmptyTaskID(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	defer mr.Close()

	scheduler, err := NewTaskScheduler(mr.Addr())
	if err != nil {
		t.Fatalf("failed to create scheduler: %v", err)
	}
	impl := scheduler.(*Impl)
	defer impl.Close()

	ctx := context.Background()
	taskType := "test:task"
	payload := []byte(`{"test": "data"}`)
	delay := 1 * time.Minute

	// Schedule with empty task ID: should generate unique ID
	id1, err := scheduler.ScheduleDelayed(ctx, taskType, payload, delay, "")
	if err != nil {
		t.Fatalf("first schedule failed: %v", err)
	}
	if id1 == "" {
		t.Error("first schedule returned empty task ID; want non-empty")
	}

	// Schedule again with empty task ID: should generate different ID
	id2, err := scheduler.ScheduleDelayed(ctx, taskType, payload, delay, "")
	if err != nil {
		t.Fatalf("second schedule failed: %v", err)
	}
	if id2 == "" {
		t.Error("second schedule returned empty task ID; want non-empty")
	}
	if id1 == id2 {
		t.Errorf("both schedules returned same task ID %q; want different IDs", id1)
	}
}

// TestScheduleDelayed_AfterTaskCompleted verifies that scheduling with the
// same task ID succeeds after the task has been completed/removed.
func TestScheduleDelayed_AfterTaskCompleted(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	defer mr.Close()

	scheduler, err := NewTaskScheduler(mr.Addr())
	if err != nil {
		t.Fatalf("failed to create scheduler: %v", err)
	}
	impl := scheduler.(*Impl)
	defer impl.Close()

	ctx := context.Background()
	taskType := "test:task"
	payload := []byte(`{"test": "data"}`)
	delay := 1 * time.Minute
	taskID := "test-task-id-456"

	// First schedule: should succeed
	_, err = scheduler.ScheduleDelayed(ctx, taskType, payload, delay, taskID)
	if err != nil {
		t.Fatalf("first schedule failed: %v", err)
	}

	// Delete the task (simulating completion)
	err = impl.inspector.DeleteTask(LoopQueue, taskID)
	if err != nil {
		t.Fatalf("failed to delete task: %v", err)
	}

	// Schedule again with same task ID: should succeed (task no longer exists)
	id2, err := scheduler.ScheduleDelayed(ctx, taskType, payload, delay, taskID)
	if err != nil {
		t.Fatalf("second schedule after deletion failed: %v", err)
	}
	if id2 != taskID {
		t.Errorf("second schedule returned task ID %q; want %q", id2, taskID)
	}
}

// TestScheduleDelayed_ConcurrentScheduling verifies that concurrent scheduling
// with the same task ID is safe (only one succeeds).
func TestScheduleDelayed_ConcurrentScheduling(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	defer mr.Close()

	scheduler, err := NewTaskScheduler(mr.Addr())
	if err != nil {
		t.Fatalf("failed to create scheduler: %v", err)
	}
	impl := scheduler.(*Impl)
	defer impl.Close()

	ctx := context.Background()
	taskType := "test:task"
	payload := []byte(`{"test": "data"}`)
	delay := 1 * time.Minute
	taskID := "concurrent-task-id"

	// Schedule concurrently from multiple goroutines
	const numGoroutines = 5
	results := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			_, err := scheduler.ScheduleDelayed(ctx, taskType, payload, delay, taskID)
			results <- err
		}()
	}

	// Collect results
	successCount := 0
	conflictCount := 0
	for i := 0; i < numGoroutines; i++ {
		err := <-results
		if err == nil {
			successCount++
		} else if errors.Is(err, hibikenasynq.ErrTaskIDConflict) {
			conflictCount++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}

	// Exactly one should succeed, others should get ErrTaskIDConflict
	if successCount != 1 {
		t.Errorf("success count = %d; want 1", successCount)
	}
	if conflictCount != numGoroutines-1 {
		t.Errorf("conflict count = %d; want %d", conflictCount, numGoroutines-1)
	}
}
