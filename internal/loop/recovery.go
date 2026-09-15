package loop

import (
	"context"
	"encoding/json"
	"time"

	hibikenasynq "github.com/hibiken/asynq"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/taskscheduler"
	"github.com/rtc-agent/server/pkg/logger"
)

// RecoveryDeps holds the dependencies for the Loop Recovery goroutine.
type RecoveryDeps struct {
	LoopRepo  repo.LoopRepo
	Client    *hibikenasynq.Client
	Inspector *hibikenasynq.Inspector
	Interval  time.Duration
}

// RunRecovery starts the loop recovery goroutine.
// It periodically scans for stale and expired loops, re-enqueuing as needed.
//
// The goroutine exits when ctx is cancelled.
func RunRecovery(ctx context.Context, deps RecoveryDeps) {
	ticker := time.NewTicker(deps.Interval)
	defer ticker.Stop()

	logger.Info(ctx, "[loop.Recovery] started",
		zap.Duration("interval", deps.Interval))

	for {
		select {
		case <-ctx.Done():
			logger.Info(ctx, "[loop.Recovery] stopped")
			return
		case <-ticker.C:
			scanAndRecover(ctx, deps)
		}
	}
}

// scanAndRecover performs one recovery scan.
func scanAndRecover(ctx context.Context, deps RecoveryDeps) {
	// 1. Handle expired loops
	recoverExpired(ctx, deps)

	// 2. Handle stale loops (missing asynq task)
	recoverStale(ctx, deps)
}

// recoverExpired marks expired loops as cancelled.
func recoverExpired(ctx context.Context, deps RecoveryDeps) {
	expired, err := deps.LoopRepo.FindExpiredLoops(ctx)
	if err != nil {
		logger.Error(ctx, "[loop.Recovery] find expired loops",
			zap.Error(err))
		return
	}

	for _, loop := range expired {
		// Cancel asynq task if present (best effort)
		if loop.AsynqTaskID != "" {
			_ = deps.Inspector.DeleteTask(taskscheduler.LoopQueue, loop.AsynqTaskID)
		}

		// Mark as cancelled
		reason := "expired"
		if err := deps.LoopRepo.Update(ctx, loop.ID, map[string]any{
			"status":      model.LoopStatusCancelled,
			"last_reason": &reason,
		}); err != nil {
			logger.Error(ctx, "[loop.Recovery] cancel expired loop",
				zap.String("loop_id", loop.ID.String()),
				zap.Error(err))
			continue
		}

		logger.Info(ctx, "[loop.Recovery] cancelled expired loop",
			zap.String("loop_id", loop.ID.String()),
			zap.String("session_id", loop.SessionID.String()))
	}
}

// staleLoopThreshold is the duration used to determine if a loop is stale.
// An active loop that has not produced an asynq task within this window is
// considered stale and will be re-enqueued.
const staleLoopThreshold = 5 * time.Minute

// recoverStale re-enqueues loops that are stale (active but missing asynq task).
func recoverStale(ctx context.Context, deps RecoveryDeps) {
	staleThreshold := time.Now().Add(-staleLoopThreshold)

	stale, err := deps.LoopRepo.FindStaleLoops(ctx, staleThreshold)
	if err != nil {
		logger.Error(ctx, "[loop.Recovery] find stale loops",
			zap.Error(err))
		return
	}

	for _, loop := range stale {
		reenqueueLoop(ctx, deps, loop)
	}
}

// reenqueueLoop re-enqueues a single stale loop.
func reenqueueLoop(ctx context.Context, deps RecoveryDeps, loop *model.Loop) {
	payload, err := json.Marshal(struct {
		LoopID    string `json:"loop_id"`
		SessionID string `json:"session_id"`
	}{
		LoopID:    loop.ID.String(),
		SessionID: loop.SessionID.String(),
	})
	if err != nil {
		logger.Error(ctx, "[loop.Recovery] marshal payload",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		return
	}

	delay := time.Duration(loop.IntervalSeconds) * time.Second
	task := hibikenasynq.NewTask(LoopTaskType, payload)
	info, err := deps.Client.EnqueueContext(ctx, task,
		hibikenasynq.ProcessIn(delay),
		hibikenasynq.Queue(taskscheduler.LoopQueue),
		hibikenasynq.MaxRetry(3),
	)
	if err != nil {
		logger.Error(ctx, "[loop.Recovery] reenqueue failed",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		return
	}

	// Update the loop with the new task ID
	if err := deps.LoopRepo.Update(ctx, loop.ID, map[string]any{
		"asynq_task_id": info.ID,
	}); err != nil {
		logger.Error(ctx, "[loop.Recovery] update task_id failed",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		return
	}

	logger.Info(ctx, "[loop.Recovery] re-enqueued stale loop",
		zap.String("loop_id", loop.ID.String()),
		zap.String("session_id", loop.SessionID.String()),
		zap.String("task_id", info.ID))
}

// LoopTaskType is the asynq task type for loop execution.
const LoopTaskType = "loop:execute"
