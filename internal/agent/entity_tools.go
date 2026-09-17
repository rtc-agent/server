package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ---------------------------------------------------------------------------
// Goal / Loop mutual exclusion
// ---------------------------------------------------------------------------

// checkGoalLoopMutualExclusion verifies that creating a new entity of
// entityType ("goal" or "loop") does not conflict with an existing active
// entity of the opposite type. Returns an error string (caller-friendly)
// if a conflict exists, or empty string if creation is allowed.
//
// entityType must be "goal" or "loop".
func checkGoalLoopMutualExclusion(
	ctx context.Context,
	deps *usecase.Dependencies,
	sessionID uuid.UUID,
	entityType string,
) (conflictMsg string, err error) {
	switch entityType {
	case "goal":
		// Creating a goal: check for active loop.
		if deps.LoopRepo != nil {
			activeLoop, err := deps.LoopRepo.FindActive(ctx, sessionID)
			if err != nil {
				return "", fmt.Errorf("find active loop: %w", err)
			}
			if activeLoop != nil {
				return fmt.Sprintf("Error: an active loop exists (id=%s). Goal and loop cannot be active simultaneously. Complete or cancel the loop first.",
					activeLoop.ID.String()), nil
			}
		}
	case "loop":
		// Creating/resuming a loop: check for active goal.
		if deps.GoalRepo != nil {
			activeGoal, err := deps.GoalRepo.FindActive(ctx, sessionID)
			if err != nil {
				return "", fmt.Errorf("find active goal: %w", err)
			}
			if activeGoal != nil {
				return fmt.Sprintf("Error: an active goal exists (id=%s). Loop and goal cannot be active simultaneously. Complete or cancel the goal first.",
					activeGoal.ID.String()), nil
			}
		}
	default:
		return "", fmt.Errorf("unknown entity type: %s", entityType)
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// Loop asynq task cancellation
// ---------------------------------------------------------------------------

// cancelLoopAsynqTask cancels the scheduled asynq task for a loop (if
// TaskScheduler is available and the loop has a task ID). On success it
// adds `"asynq_task_id": ""` to updateFields so the caller can pass it
// directly to the repository Update call.
//
// If cancellation fails, the error is logged but not returned — the
// caller should continue with the status update regardless.
func cancelLoopAsynqTask(
	ctx context.Context,
	deps *usecase.Dependencies,
	logger turnagent.Logger,
	loop *model.Loop,
	updateFields map[string]any,
) {
	if deps.TaskScheduler == nil || loop.AsynqTaskID == "" {
		return
	}
	if cancelErr := deps.TaskScheduler.Cancel(ctx, loop.AsynqTaskID); cancelErr != nil {
		logger.Warn(ctx, "cancelLoopAsynqTask.failed", map[string]any{
			"loop_id": loop.ID.String(),
			"task_id": loop.AsynqTaskID,
			"error":   cancelErr.Error(),
			"message": "asynq task cancel failed; task may still execute even though loop is cancelled",
		})
		// Continue — status update proceeds even if task cancellation fails.
	}
	updateFields["asynq_task_id"] = ""
}
