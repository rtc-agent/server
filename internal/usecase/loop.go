// Package usecase provides business logic implementations.
package usecase

import (
	"context"

	hibikenasynq "github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// loopQueueName is the queue name for loop tasks.
// Defined here to avoid import cycle with taskscheduler package.
const loopQueueName = "loop"

// LoopUsecase handles loop-related business logic.
type LoopUsecase struct {
	loopRepo LoopRepo
	goalRepo GoalRepo
}

// NewLoopUsecase creates a new LoopUsecase.
func NewLoopUsecase(loopRepo LoopRepo, goalRepo GoalRepo) *LoopUsecase {
	return &LoopUsecase{
		loopRepo: loopRepo,
		goalRepo: goalRepo,
	}
}

// cleanupTracer returns the tracer for loop cleanup operations.
func cleanupTracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("loop.cleanup")
}

// CancelOrphanedLoops cancels all active loops and goals for a session.
// Called during session close to ensure no orphaned asynq tasks remain.
//
// Steps:
//  1. Find active loop for the session
//  2. Cancel the asynq task (best-effort, ignore errors)
//  3. Update loop status to cancelled
//  4. Also cancels active Goals for the session (fixing an existing defect)
func (uc *LoopUsecase) CancelOrphanedLoops(ctx context.Context, inspector *hibikenasynq.Inspector, sessionID uuid.UUID) {
	// Cancel active loop
	uc.cancelActiveLoop(ctx, inspector, sessionID)

	// Cancel active goal (fix existing defect)
	uc.cancelActiveGoal(ctx, sessionID)
}

// cancelActiveLoop finds and cancels the active loop for a session.
// NOTE: This is duplicated from loop/cleanup.go:cancelActiveLoop.
// Both loop and usecase packages need this logic but cannot share due to package boundaries.
// loop/cleanup.go uses repo.LoopRepo directly, while usecase uses LoopRepo (alias for repo.LoopRepo).
// Both implementations must stay in sync. If you change one, please update the other.
func (uc *LoopUsecase) cancelActiveLoop(ctx context.Context, inspector *hibikenasynq.Inspector, sessionID uuid.UUID) {
	ctx, span := cleanupTracer().Start(ctx, "loopCleanup.cancelActiveLoop",
		trace.WithAttributes(
			attribute.String("session.id", sessionID.String()),
		),
	)
	defer span.End()

	loop, err := uc.loopRepo.FindActive(ctx, sessionID)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Cleanup] find active loop",
			zap.String("session_id", sessionID.String()),
			zap.Error(err))
		return
	}
	if loop == nil {
		return
	}

	span.SetAttributes(attribute.String("loop.id", loop.ID.String()))

	// Cancel asynq task (best-effort — task may already be completed or gone)
	// Use loop.ID as task ID since we use it for idempotent scheduling
	if inspector != nil {
		taskID := loop.ID.String()
		if err := inspector.DeleteTask(loopQueueName, taskID); err != nil {
			logger.Debug(ctx, "[loop.Cleanup] delete asynq task failed (best effort)",
				zap.String("loop_id", loop.ID.String()),
				zap.String("task_id", taskID),
				zap.Error(err))
		}
	}

	// Update status to cancelled
	reason := "session closed"
	if err := uc.loopRepo.Update(ctx, loop.ID, map[string]any{
		"status":      model.LoopStatusCancelled,
		"last_reason": &reason,
	}); err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
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
func (uc *LoopUsecase) cancelActiveGoal(ctx context.Context, sessionID uuid.UUID) {
	if uc.goalRepo == nil {
		return
	}

	ctx, span := cleanupTracer().Start(ctx, "loopCleanup.cancelActiveGoal",
		trace.WithAttributes(
			attribute.String("session.id", sessionID.String()),
		),
	)
	defer span.End()

	goal, err := uc.goalRepo.FindActive(ctx, sessionID)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Cleanup] find active goal",
			zap.String("session_id", sessionID.String()),
			zap.Error(err))
		return
	}
	if goal == nil {
		return
	}

	span.SetAttributes(attribute.String("goal.id", goal.ID.String()))

	reason := "session closed"
	if err := uc.goalRepo.Update(ctx, goal.ID, map[string]any{
		"status":      model.GoalStatusCancelled,
		"last_reason": &reason,
	}); err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Cleanup] cancel goal",
			zap.String("goal_id", goal.ID.String()),
			zap.Error(err))
		return
	}

	logger.Info(ctx, "[loop.Cleanup] cancelled active goal",
		zap.String("goal_id", goal.ID.String()),
		zap.String("session_id", sessionID.String()))
}
