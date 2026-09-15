package repo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/rtc-agent/server/internal/model"
)

// ─── autoFillCompletedAt Tests ───

func TestAutoFillCompletedAt_TerminalStatus_SetsCompletedAt(t *testing.T) {
	t.Parallel()

	goalTerminals := []any{
		model.GoalStatusCompleted,
		model.GoalStatusCancelled,
		model.GoalStatusExhausted,
	}

	for _, status := range goalTerminals {
		t.Run(string(status.(model.GoalStatus)), func(t *testing.T) {
			t.Parallel()
			fields := map[string]any{"status": status}
			before := time.Now()
			autoFillCompletedAt(fields, goalTerminals)
			after := time.Now()

			assert.NotNil(t, fields["completed_at"], "completed_at should be set for terminal status %s", status)

			filledAt, ok := fields["completed_at"].(time.Time)
			assert.True(t, ok, "completed_at should be time.Time")
			assert.True(t, !filledAt.Before(before) && !filledAt.After(after),
				"completed_at should be between before and after")
		})
	}
}

func TestAutoFillCompletedAt_NonTerminalStatus_NoOp(t *testing.T) {
	t.Parallel()

	terminalStatuses := []any{
		model.GoalStatusCompleted,
		model.GoalStatusCancelled,
		model.GoalStatusExhausted,
	}

	fields := map[string]any{"status": model.GoalStatusActive}
	autoFillCompletedAt(fields, terminalStatuses)
	assert.Nil(t, fields["completed_at"], "completed_at should NOT be set for active status")
}

func TestAutoFillCompletedAt_NoStatusField_NoOp(t *testing.T) {
	t.Parallel()

	terminalStatuses := []any{model.GoalStatusCompleted}

	// No "status" key at all
	fields := map[string]any{"completed_turns": 5}
	autoFillCompletedAt(fields, terminalStatuses)
	assert.Nil(t, fields["completed_at"])
}

func TestAutoFillCompletedAt_ExplicitCompletedAt_NotOverwritten(t *testing.T) {
	t.Parallel()

	terminalStatuses := []any{model.GoalStatusCompleted}

	explicitTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	fields := map[string]any{
		"status":       model.GoalStatusCompleted,
		"completed_at": explicitTime,
	}

	autoFillCompletedAt(fields, terminalStatuses)

	// The explicit value should be preserved (not overwritten)
	got := fields["completed_at"].(time.Time)
	assert.Equal(t, explicitTime, got, "explicit completed_at should not be overwritten")
}

func TestAutoFillCompletedAt_EmptyTerminalStatuses_NoOp(t *testing.T) {
	t.Parallel()

	fields := map[string]any{"status": model.GoalStatusCompleted}
	autoFillCompletedAt(fields, []any{})
	assert.Nil(t, fields["completed_at"], "with empty terminal statuses, no fill should occur")
}

func TestAutoFillCompletedAt_LoopTerminalStatuses(t *testing.T) {
	t.Parallel()

	loopTerminals := []any{
		model.LoopStatusCompleted,
		model.LoopStatusCancelled,
		model.LoopStatusExhausted,
	}

	for _, status := range loopTerminals {
		t.Run(string(status.(model.LoopStatus)), func(t *testing.T) {
			t.Parallel()
			fields := map[string]any{"status": status}
			autoFillCompletedAt(fields, loopTerminals)
			assert.NotNil(t, fields["completed_at"], "completed_at should be set for loop terminal status %s", status)
		})
	}
}

func TestAutoFillCompletedAt_LoopPausedStatus_NoOp(t *testing.T) {
	t.Parallel()

	loopTerminals := []any{
		model.LoopStatusCompleted,
		model.LoopStatusCancelled,
		model.LoopStatusExhausted,
	}

	fields := map[string]any{"status": model.LoopStatusPaused}
	autoFillCompletedAt(fields, loopTerminals)
	assert.Nil(t, fields["completed_at"], "paused is not a terminal status for loops")
}

func TestAutoFillCompletedAt_OnlyFillsOnce(t *testing.T) {
	t.Parallel()

	terminalStatuses := []any{model.GoalStatusCompleted}

	fields := map[string]any{"status": model.GoalStatusCompleted}
	autoFillCompletedAt(fields, terminalStatuses)
	firstFill := fields["completed_at"]
	assert.NotNil(t, firstFill)

	// Calling again should NOT overwrite the already-set value
	autoFillCompletedAt(fields, terminalStatuses)
	secondFill := fields["completed_at"]
	assert.Equal(t, firstFill, secondFill, "second call should not overwrite completed_at")
}
