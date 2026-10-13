package loop

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	hibikenasynq "github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/taskscheduler"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// RecoveryDeps holds the dependencies for the Loop Recovery goroutine.
type RecoveryDeps struct {
	LoopRepo       usecase.LoopRepo
	Client         *hibikenasynq.Client
	Inspector      *hibikenasynq.Inspector
	Redis          *redis.Client
	Interval       time.Duration
	StaleThreshold time.Duration
	RetryMax       int
}

// recoveryTracer returns the tracer for loop recovery operations.
func recoveryTracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("loop.recovery")
}

// recoveryLockKey is the Redis key for the recovery goroutine distributed lock.
// Prevents multiple instances from running recovery scans concurrently.
const recoveryLockKey = "loop:recovery:lock"

// RunRecovery starts the loop recovery goroutine.
// It periodically scans for stale and expired loops, re-enqueuing as needed.
//
// The goroutine exits when ctx is cancelled.
// In multi-instance deployments, a distributed lock ensures only one instance
// runs the recovery scan at a time.
func RunRecovery(ctx context.Context, deps RecoveryDeps) {
	ticker := time.NewTicker(deps.Interval)
	defer ticker.Stop()

	logger.Info(ctx, "[loop.Recovery] started",
		zap.Duration("interval", deps.Interval),
		zap.Duration("stale_threshold", deps.StaleThreshold))

	for {
		select {
		case <-ctx.Done():
			logger.Info(ctx, "[loop.Recovery] stopped")
			return
		case <-ticker.C:
			scanAndRecoverWithLock(ctx, deps)
		}
	}
}

// scanAndRecoverWithLock acquires a distributed lock, performs recovery scan,
// and releases the lock. Extracted to ensure defer executes after each scan,
// not when RunRecovery returns.
func scanAndRecoverWithLock(ctx context.Context, deps RecoveryDeps) {
	// Acquire distributed lock to prevent multiple instances from
	// running recovery scans concurrently. Lock TTL is slightly less
	// than the interval to ensure it expires before the next scan.
	lockTTL := deps.Interval - 1*time.Second
	if lockTTL < 10*time.Second {
		if deps.Interval > 0 {
			logger.Warn(ctx, "[loop.Recovery] interval too short for safe distributed lock",
				zap.Duration("interval", deps.Interval),
				zap.Duration("lock_ttl", lockTTL))
		}
		lockTTL = max(deps.Interval*2, 30*time.Second)
	}

	// Generate a unique holder ID to verify lock ownership during release.
	// This prevents one instance from accidentally releasing another instance's lock.
	holderID := uuid.NewString()

	lockAcquired, err := deps.Redis.SetNX(ctx, recoveryLockKey, holderID, lockTTL).Result()
	if err != nil {
		logger.Error(ctx, "[loop.Recovery] acquire distributed lock failed",
			zap.Error(err))
		return
	}
	if !lockAcquired {
		// Another instance is running recovery, skip this cycle
		logger.Debug(ctx, "[loop.Recovery] skipping scan (another instance holds lock)")
		return
	}

	// Release lock atomically using Lua script that verifies holder identity.
	// This prevents accidentally deleting another instance's lock if our lock
	// expired due to GC pause or network delay before we finished the scan.
	defer func() {
		releaseLua := `
local holder = ARGV[1]
local current = redis.call('GET', KEYS[1])
if current ~= holder then
    return 0
end
return redis.call('DEL', KEYS[1])
`
		if err := deps.Redis.Eval(ctx, releaseLua, []string{recoveryLockKey}, holderID).Err(); err != nil {
			logger.Error(ctx, "[loop.Recovery] release distributed lock failed",
				zap.Error(err))
		}
	}()

	scanAndRecover(ctx, deps)
}

// scanAndRecover performs one recovery scan.
func scanAndRecover(ctx context.Context, deps RecoveryDeps) {
	ctx, span := recoveryTracer().Start(ctx, "loopRecovery.scanAndRecover")
	defer span.End()

	// 1. Handle expired loops
	recoverExpired(ctx, deps)

	// 2. Handle stale loops (missing asynq task)
	recoverStale(ctx, deps)

	// Record recovery scan for observability
	recordRecoveryScan()
}

// recoverExpired marks expired loops as cancelled.
func recoverExpired(ctx context.Context, deps RecoveryDeps) {
	ctx, span := recoveryTracer().Start(ctx, "loopRecovery.scanExpired")
	defer span.End()

	expired, err := deps.LoopRepo.FindExpiredLoops(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Recovery] find expired loops",
			zap.Error(err))
		return
	}

	span.SetAttributes(attribute.Int("loop.count", len(expired)))

	for _, loop := range expired {
		// Cancel asynq task if present (best effort — task may already be gone)
		// Use loop.ID as task ID since we use it for idempotent scheduling
		taskID := loop.ID.String()
		if err := deps.Inspector.DeleteTask(taskscheduler.LoopQueue, taskID); err != nil {
			logger.Debug(ctx, "[loop.Recovery] delete asynq task failed (best effort)",
				zap.String("loop_id", loop.ID.String()),
				zap.String("task_id", taskID),
				zap.Error(err))
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

// staleLoopThreshold is the default duration used to determine if a loop is stale.
// An active loop that has not produced an asynq task within this window is
// considered stale and will be re-enqueued. Used as fallback when
// RecoveryDeps.StaleThreshold is not configured.
const staleLoopThreshold = 5 * time.Minute

// recoverStale re-enqueues loops that are stale (active but missing asynq task).
func recoverStale(ctx context.Context, deps RecoveryDeps) {
	ctx, span := recoveryTracer().Start(ctx, "loopRecovery.scanStale")
	defer span.End()

	// Use configured StaleThreshold, fallback to default if not set
	threshold := deps.StaleThreshold
	if threshold <= 0 {
		threshold = staleLoopThreshold
	}
	staleThreshold := time.Now().Add(-threshold)

	stale, err := deps.LoopRepo.FindStaleLoops(ctx, staleThreshold)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Recovery] find stale loops",
			zap.Error(err))
		return
	}

	span.SetAttributes(attribute.Int("loop.count", len(stale)))

	for _, loop := range stale {
		reenqueueLoop(ctx, deps, loop)
	}
}

// maxRecoveryAttempts is the maximum number of times a loop can be re-enqueued
// before being marked as exhausted. Prevents infinite recovery loops when a
// loop consistently fails due to fundamental errors.
const maxRecoveryAttempts = 5

// defaultRetryMax is the default maximum number of retries for asynq tasks.
const defaultRetryMax = 3

// recoveryEnqueueTimeout is the timeout for enqueuing a recovery task to asynq.
// This prevents infinite blocking if Redis is unresponsive during recovery.
const recoveryEnqueueTimeout = 10 * time.Second

// reenqueueLoop re-enqueues a single stale loop.
func reenqueueLoop(ctx context.Context, deps RecoveryDeps, loop *model.Loop) {
	ctx, span := recoveryTracer().Start(ctx, "loopRecovery.reenqueue",
		trace.WithAttributes(
			attribute.String("loop.id", loop.ID.String()),
			attribute.String("session.id", loop.SessionID.String()),
		),
	)
	defer span.End()

	// Check if max recovery attempts has been reached
	if loop.RecoveryAttempts >= maxRecoveryAttempts {
		logger.Error(ctx, "[loop.Recovery] max recovery attempts reached",
			zap.String("loop_id", loop.ID.String()),
			zap.Int("attempts", loop.RecoveryAttempts))
		recordRecoveryExhausted()
		if err := deps.LoopRepo.Update(ctx, loop.ID, map[string]any{
			"status": model.LoopStatusExhausted,
		}); err != nil {
			logger.Error(ctx, "[loop.Recovery] mark loop as exhausted failed",
				zap.String("loop_id", loop.ID.String()),
				zap.Error(err))
		}
		return
	}

	// Extract trace context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)

	payload, err := json.Marshal(struct {
		LoopID    string `json:"loop_id"`
		SessionID string `json:"session_id"`
		TraceID   string `json:"trace_id,omitempty"`
		SpanID    string `json:"span_id,omitempty"`
	}{
		LoopID:    loop.ID.String(),
		SessionID: loop.SessionID.String(),
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Recovery] marshal payload",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		recordRecoveryFailure()
		return
	}

	// Calculate delay based on actual next run time with exponential backoff.
	// If the loop has missed its scheduled execution, use exponential backoff
	// based on recovery_attempts to prevent overwhelming the system with
	// repeated failures. Otherwise, wait until the next scheduled run time.
	var delay time.Duration
	if loop.LastRunAt.IsZero() {
		// Never run before, use full interval
		delay = time.Duration(loop.IntervalSeconds) * time.Second
	} else {
		nextRunTime := loop.LastRunAt.Add(time.Duration(loop.IntervalSeconds) * time.Second)
		if nextRunTime.Before(time.Now()) {
			// Already missed - apply exponential backoff based on recovery attempts.
			// This prevents a consistently failing loop from overwhelming the system.
			baseDelay := time.Duration(loop.IntervalSeconds) * time.Second
			maxBackoff := 1 * time.Hour // Cap at 1 hour to prevent excessive delays

			// Fix(P0-2): Prevent overflow in exponential backoff calculation.
			// The formula is: baseDelay * (1 << recoveryAttempts).
			// Before multiplying, we ensure baseDelay doesn't exceed the maximum
			// safe value that won't overflow when multiplied by 1<<maxSafeShift.
			const maxSafeShift = 20
			maxSafeBaseDelay := maxBackoff / time.Duration(1<<uint(maxSafeShift))
			if baseDelay > maxSafeBaseDelay {
				baseDelay = maxSafeBaseDelay
			}

			var backoff time.Duration
			if loop.RecoveryAttempts >= maxSafeShift {
				backoff = maxBackoff
			} else {
				backoff = baseDelay * time.Duration(1<<uint(loop.RecoveryAttempts))
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			delay = backoff
		} else {
			delay = time.Until(nextRunTime)
		}
	}

	task := hibikenasynq.NewTask(LoopTaskType, payload)

	retryMax := deps.RetryMax
	if retryMax < 0 {
		retryMax = defaultRetryMax
	}

	// Set timeout for Redis operation to prevent infinite blocking.
	opCtx, cancel := context.WithTimeout(ctx, recoveryEnqueueTimeout)
	defer cancel()

	// Use loop ID as task ID for idempotency (same as scheduleNextLoop).
	// If multiple recovery instances try to reenqueue the same stale loop,
	// only one will succeed; others will get ErrTaskIDConflict (handled below).
	info, err := deps.Client.EnqueueContext(opCtx, task,
		hibikenasynq.ProcessIn(delay),
		hibikenasynq.Queue(taskscheduler.LoopQueue),
		hibikenasynq.MaxRetry(retryMax),
		hibikenasynq.TaskID(loop.ID.String()),
	)
	if err != nil {
		// In multi-instance deployment, multiple recovery goroutines may attempt
		// to reenqueue the same stale loop. ErrTaskIDConflict is expected and safe -
		// log as Info and return gracefully (not as an error).
		if errors.Is(err, hibikenasynq.ErrTaskIDConflict) {
			span.SetAttributes(attribute.String("task.idempotent", "true"))
			logger.Info(ctx, "[loop.Recovery] task already exists (idempotent)",
				zap.String("loop_id", loop.ID.String()),
				zap.Error(err))
			return
		}
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Recovery] reenqueue failed",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		recordRecoveryFailure()
		return
	}

	span.SetAttributes(attribute.String("task.id", info.ID))

	// Note: asynq_task_id is not updated here because we use loop.ID as the task ID
	// for idempotent scheduling (set in scheduleNextLoop and reenqueueLoop via TaskID option).
	// This simplifies state management and avoids race conditions.
	//
	// Only increment recovery_attempts to track how many times this loop has been re-enqueued.
	if err := deps.LoopRepo.IncrementRecoveryAttempts(ctx, loop.ID); err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logger.Error(ctx, "[loop.Recovery] increment recovery_attempts failed",
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		recordRecoveryFailure()
		return
	}

	recordRecoverySuccess()

	logger.Info(ctx, "[loop.Recovery] re-enqueued stale loop",
		zap.String("loop_id", loop.ID.String()),
		zap.String("session_id", loop.SessionID.String()),
		zap.String("task_id", info.ID))
}

// LoopTaskType is the asynq task type for loop execution.
const LoopTaskType = "loop:execute"
