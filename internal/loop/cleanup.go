package loop

import (
	"context"

	hibikenasynq "github.com/hibiken/asynq"
	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/taskscheduler"
	"github.com/rtc-agent/server/pkg/logger"
)

// CancelAllForSession cancels all active loops and goals for a session.
// Called during session close to ensure no orphaned asynq tasks remain.
//
// Steps:
//  1. Find active loop for the session
//  2. Cancel the asynq task (best-effort, ignore errors)
//  3. Update loop status to cancelled
//  4. Also cancels active Goals for the session (fixing an existing defect)
func CancelAllForSession(ctx context.Context, loopRepo repo.LoopRepo, goalRepo repo.GoalRepo, inspector *hibikenasynq.Inspector, sessionID uuid.UUID) {
	// Cancel active loop
	cancelActiveLoop(ctx, loopRepo, inspector, sessionID)

	// Cancel active goal (fix existing defect)
	cancelActiveGoal(ctx, goalRepo, sessionID)
}

// cancelActiveLoop finds and cancels the active loop for a session.
func cancelActiveLoop(ctx context.Context, loopRepo repo.LoopRepo, inspector *hibikenasynq.Inspector, sessionID uuid.UUID) {
	loop, err := loopRepo.FindActive(ctx, sessionID)
	if err != nil {
		logger.Error(ctx, "[loop.Cleanup] find active loop",
			zap.String("session_id", sessionID.String()),
			zap.Error(err))
		return
	}
	if loop == nil {
		return
	}

	// Cancel asynq task (best-effort)
	if loop.AsynqTaskID != "" && inspector != nil {
		_ = inspector.DeleteTask(taskscheduler.LoopQueue, loop.AsynqTaskID)
	}

	// Update status to cancelled
	reason := "session closed"
	if err := loopRepo.Update(ctx, loop.ID, map[string]any{
		"status":      model.LoopStatusCancelled,
		"last_reason": &reason,
	}); err != nil {
		logger.Error(ctx, "[loop.Cleanup] cancel loop",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		return
	}

	logger.Info(ctx, "[loop.Cleanup] cancelled active loop",
		zap.String("loop_id", loop.ID.String()),
		zap.String("session_id", sessionID.String()))
}

// cancelActiveGoal cancels the active goal for a session.
// This fixes an existing defect where goals were not cancelled on session close.
func cancelActiveGoal(ctx context.Context, goalRepo repo.GoalRepo, sessionID uuid.UUID) {
	if goalRepo == nil {
		return
	}
	goal, err := goalRepo.FindActive(ctx, sessionID)
	if err != nil {
		logger.Error(ctx, "[loop.Cleanup] find active goal",
			zap.String("session_id", sessionID.String()),
			zap.Error(err))
		return
	}
	if goal == nil {
		return
	}

	reason := "session closed"
	if err := goalRepo.Update(ctx, goal.ID, map[string]any{
		"status":      model.GoalStatusCancelled,
		"last_reason": &reason,
	}); err != nil {
		logger.Error(ctx, "[loop.Cleanup] cancel goal",
			zap.String("goal_id", goal.ID.String()),
			zap.Error(err))
		return
	}

	logger.Info(ctx, "[loop.Cleanup] cancelled active goal",
		zap.String("goal_id", goal.ID.String()),
		zap.String("session_id", sessionID.String()))
}
