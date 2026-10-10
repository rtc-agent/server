// Package loop implements the Loop command's background task processing.
//
// Worker handles asynq tasks for loop execution, Recovery monitors for stale
// or expired loops, and Cleanup handles session close cleanup.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// workerTracer returns the tracer for loop worker operations.
func workerTracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("loop")
}

// NotificationCreator creates a user-role notification message in the session.
// This follows the async sub-agent pattern: insert a message into the conversation
// history before triggering a turn, so the LLM sees the notification as the
// most recent user message.
type NotificationCreator func(ctx context.Context, sessionID uuid.UUID, prompt string) error

// Worker processes loop asynq tasks.
type Worker struct {
	queue               *rtcqueue.Queue
	loopRepo            usecase.LoopRepo
	sessionRepo         usecase.SessionRepo
	notificationCreator NotificationCreator
	redis               redis.UniversalClient
}

// NewWorker creates a loop worker.
func NewWorker(queue *rtcqueue.Queue, loopRepo usecase.LoopRepo, sessionRepo usecase.SessionRepo, notificationCreator NotificationCreator, redisClient redis.UniversalClient) *Worker {
	return &Worker{
		queue:               queue,
		loopRepo:            loopRepo,
		sessionRepo:         sessionRepo,
		notificationCreator: notificationCreator,
		redis:               redisClient,
	}
}

// loopTaskTimeout is the overall timeout for processing a loop task.
// This prevents infinite blocking if downstream services hang.
// The timeout covers: payload parsing, loop validation, notification creation,
// and publishing to rtc-queue. It does NOT cover actual LLM execution (which
// happens in a separate worker triggered by the published work item).
const loopTaskTimeout = 30 * time.Second

// HandleLoopTask processes loop:execute tasks.
//
// Idempotency: safe to call multiple times. If the loop no longer exists,
// is not active, or the ID doesn't match, returns nil without side effects.
//
// Concurrency safety: Uses a Redis distributed lock (keyed by loop.ID) to prevent
// TOCTOU race conditions where multiple workers read the same loop state and both
// pass the idempotency check. The lock ensures only one worker processes the task at a time.
//
// Following the async sub-agent pattern:
//  1. Acquire distributed lock (prevents concurrent execution)
//  2. Validate loop is still active
//  3. Check idempotency (secondary safeguard after lock)
//  4. Build the loop task prompt
//  5. Create a user-role notification message in the session (so it becomes
//     the last user message in conversation history)
//  6. Publish WorkKindSubmit to trigger a new turn
//  7. The LLM sees the notification as the most recent instruction
//
// Error handling: On failure, logs detailed context for debugging and traceability.
// Failed tasks are archived by asynq and can be inspected via asynqmon or manually retried.
func (w *Worker) HandleLoopTask(ctx context.Context, t *hibikenasynq.Task) error {
	ctx, cancel := context.WithTimeout(ctx, loopTaskTimeout)
	defer cancel()

	sessionUUID, loopUUID, traceID, spanID, err := parseLoopTaskPayload(ctx, t)
	if err != nil {
		return nil // Malformed payload or invalid UUIDs, don't retry
	}

	if traceID != "" && spanID != "" {
		ctx = turnagent.WithTraceContext(ctx, traceID, spanID)
	}

	ctx, span := workerTracer().Start(ctx, "loop.HandleLoopTask",
		trace.WithAttributes(
			attribute.String("session.id", sessionUUID.String()),
			attribute.String("loop.id", loopUUID.String()),
		),
	)
	defer span.End()

	// Acquire distributed lock
	lockKey := cache.LoopLockKey(loopUUID.String())
	holderUUID, acquired, err := w.acquireDistributedLock(ctx, loopUUID)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		return err
	}
	if !acquired {
		span.SetAttributes(attribute.String("outcome", "lock_not_acquired"))
		logger.Debug(ctx, "[loop.Worker] another worker is processing this loop, skipping",
			zap.String("loop_id", loopUUID.String()))
		return nil
	}
	defer func() {
		w.releaseDistributedLock(ctx, lockKey, holderUUID)
	}()

	// Validate loop state
	loop, err := w.validateLoopState(ctx, sessionUUID, loopUUID, span)
	if err != nil {
		return err
	}
	if loop == nil {
		return nil
	}

	// Idempotency check
	alreadyProcessed := w.checkIdempotency(ctx, loop, sessionUUID, loopUUID, span)
	if alreadyProcessed {
		return nil
	}

	// Build prompt and create notification
	prompt := buildLoopTaskPrompt(loop)
	if w.notificationCreator != nil {
		if err := w.notificationCreator(ctx, sessionUUID, prompt); err != nil {
			span.SetStatus(codes.Error, "internal_error")
			span.RecordError(err)
			logTaskFailure(ctx, "create notification message", sessionUUID.String(), loopUUID.String(), err)
			return fmt.Errorf("create notification message: %w", err)
		}
	}

	// Get userID from session (optional, for observability)
	userID := w.getUserIDFromSession(ctx, sessionUUID)

	// Publish to rtc-queue
	if err := w.publishToQueue(ctx, sessionUUID.String(), userID, traceID, spanID, span); err != nil {
		logTaskFailure(ctx, "publish to rtc-queue", sessionUUID.String(), loopUUID.String(), err)
		return err
	}

	// Update LastRunAt only after successful queue publish.
	// This ensures idempotency check won't skip retries if worker crashes before publish.
	now := time.Now()
	if err := w.loopRepo.Update(ctx, loopUUID, map[string]any{"last_run_at": now}); err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logTaskFailure(ctx, "update last_run_at", sessionUUID.String(), loopUUID.String(), err)
		return fmt.Errorf("update last_run_at: %w", err)
	}

	// Reset recovery_attempts on success
	if loop.RecoveryAttempts > 0 {
		if err := w.loopRepo.Update(ctx, loopUUID, map[string]any{"recovery_attempts": 0}); err != nil {
			logger.Warn(ctx, "[loop.Worker] reset recovery_attempts failed",
				zap.String("loop_id", loopUUID.String()),
				zap.Error(err))
		}
	}

	span.SetAttributes(attribute.Int("loop.turn", loop.CompletedTurns+1))
	logger.Info(ctx, "[loop.Worker] task processed successfully",
		zap.String("session_id", sessionUUID.String()),
		zap.String("user_id", userID),
		zap.String("loop_id", loop.ID.String()),
		zap.Int("turn", loop.CompletedTurns+1),
		zap.Int("max_turns", loop.MaxTurns),
		zap.Int("recovery_attempts", loop.RecoveryAttempts))

	return nil
}

// getUserIDFromSession retrieves the userID from the session for observability.
// Returns empty string if session cannot be loaded (non-fatal).
func (w *Worker) getUserIDFromSession(ctx context.Context, sessionUUID uuid.UUID) string {
	session, err := w.sessionRepo.GetByID(ctx, sessionUUID)
	if err != nil {
		logger.Warn(ctx, "[loop.Worker] get session for userID failed",
			zap.String("session_id", sessionUUID.String()),
			zap.Error(err))
		return ""
	}
	if session != nil {
		return session.OwnerRefID
	}
	return ""
}

// publishToQueue publishes the work item to the rtc-queue.
func (w *Worker) publishToQueue(ctx context.Context, sessionID, userID, traceID, spanID string, span trace.Span) error {
	workPayload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: sessionID,
		UserID:    userID,
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		return fmt.Errorf("marshal work payload: %w", err)
	}

	_, err = w.queue.Publish(ctx, sessionID, string(workPayload), rtcqueue.SubmitWorkPriority)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		return fmt.Errorf("publish to rtc-queue: %w", err)
	}

	return nil
}

// logTaskFailure logs a task failure with comprehensive context for debugging.
// This ensures failed tasks are traceable even after asynq archives them.
func logTaskFailure(ctx context.Context, errMsg string, sessionID, loopID string, err error) {
	logger.Error(ctx, "loop.worker.task_failed",
		zap.String("error_message", errMsg),
		zap.String("session_id", sessionID),
		zap.String("loop_id", loopID),
		zap.Error(err),
		zap.Time("timestamp", time.Now()),
		zap.String("task_type", LoopTaskType))
}

// parseLoopTaskPayload parses and validates the asynq task payload.
// Returns parsed UUIDs and trace context, or an error if validation fails.
func parseLoopTaskPayload(ctx context.Context, t *hibikenasynq.Task) (sessionUUID, loopUUID uuid.UUID, traceID, spanID string, err error) {
	var payload struct {
		SessionID string `json:"session_id"`
		LoopID    string `json:"loop_id"`
		TraceID   string `json:"trace_id,omitempty"`
		SpanID    string `json:"span_id,omitempty"`
	}

	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Error(ctx, "[loop.Worker] unmarshal payload",
			zap.Error(err),
			zap.String("raw_payload", string(t.Payload())))
		return uuid.Nil, uuid.Nil, "", "", fmt.Errorf("unmarshal payload: %w", err)
	}

	sessionUUID, err = uuid.Parse(payload.SessionID)
	if err != nil {
		logger.Error(ctx, "[loop.Worker] parse session ID",
			zap.String("session_id", payload.SessionID),
			zap.Error(err))
		return uuid.Nil, uuid.Nil, "", "", fmt.Errorf("parse session ID: %w", err)
	}

	loopUUID, err = uuid.Parse(payload.LoopID)
	if err != nil {
		logger.Error(ctx, "[loop.Worker] parse loop ID",
			zap.String("loop_id", payload.LoopID),
			zap.Error(err))
		return uuid.Nil, uuid.Nil, "", "", fmt.Errorf("parse loop ID: %w", err)
	}

	return sessionUUID, loopUUID, payload.TraceID, payload.SpanID, nil
}

// acquireDistributedLock acquires a Redis distributed lock for loop task processing.
// Returns the lock holder UUID and whether the lock was successfully acquired.
func (w *Worker) acquireDistributedLock(ctx context.Context, loopUUID uuid.UUID) (string, bool, error) {
	lockKey := cache.LoopLockKey(loopUUID.String())
	holderUUID := uuid.Must(uuid.NewV7()).String()
	lockTTL := loopTaskTimeout + 10*time.Second

	acquired, err := w.redis.SetNX(ctx, lockKey, holderUUID, lockTTL).Result()
	if err != nil {
		logger.Error(ctx, "[loop.Worker] acquire distributed lock failed",
			zap.String("loop_id", loopUUID.String()),
			zap.Error(err))
		return "", false, fmt.Errorf("acquire lock: %w", err)
	}

	return holderUUID, acquired, nil
}

// releaseDistributedLock releases the distributed lock using a Lua script for atomicity.
func (w *Worker) releaseDistributedLock(ctx context.Context, lockKey, holderUUID string) {
	releaseLua := `
local holder = ARGV[1]
local current = redis.call('GET', KEYS[1])
if current ~= holder then
    return 0
end
return redis.call('DEL', KEYS[1])
`
	if err := w.redis.Eval(ctx, releaseLua, []string{lockKey}, holderUUID).Err(); err != nil {
		logger.Warn(ctx, "[loop.Worker] release distributed lock failed",
			zap.Error(err))
	}
}

// validateLoopState validates that the loop is still active and matches the expected ID.
// Returns nil if the loop should be processed, or an error/skip indicator otherwise.
func (w *Worker) validateLoopState(ctx context.Context, sessionUUID, loopUUID uuid.UUID, span trace.Span) (*model.Loop, error) {
	loop, err := w.loopRepo.FindActive(ctx, sessionUUID)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logTaskFailure(ctx, "find active loop", sessionUUID.String(), loopUUID.String(), err)
		return nil, fmt.Errorf("find active loop: %w", err)
	}

	if loop == nil {
		span.SetAttributes(attribute.String("outcome", "loop_not_found"))
		logger.Debug(ctx, "[loop.Worker] loop no longer active, skipping",
			zap.String("session_id", sessionUUID.String()),
			zap.String("loop_id", loopUUID.String()))
		return nil, nil
	}

	if loop.ID != loopUUID {
		span.SetAttributes(attribute.String("outcome", "loop_id_mismatch"))
		logger.Debug(ctx, "[loop.Worker] loop ID mismatch, skipping",
			zap.String("session_id", sessionUUID.String()),
			zap.String("expected_loop_id", loopUUID.String()),
			zap.String("active_loop_id", loop.ID.String()))
		return nil, nil
	}

	return loop, nil
}

// checkIdempotency performs idempotency check to prevent duplicate processing.
// Returns true if the task was already processed and should be skipped.
func (w *Worker) checkIdempotency(ctx context.Context, loop *model.Loop, sessionUUID, loopUUID uuid.UUID, span trace.Span) bool {
	if loop.LastRunAt != nil && time.Since(*loop.LastRunAt) < time.Duration(loop.IntervalSeconds)*time.Second {
		span.SetAttributes(attribute.String("outcome", "already_processed"))
		logger.Debug(ctx, "[loop.Worker] turn already processed, skipping",
			zap.String("session_id", sessionUUID.String()),
			zap.String("loop_id", loopUUID.String()),
			zap.Time("last_run_at", *loop.LastRunAt),
			zap.Int("interval_seconds", loop.IntervalSeconds))
		return true
	}
	return false
}

// buildLoopTaskPrompt constructs the notification text for a loop task trigger.
// Following the async sub-agent pattern: user-role message with <system-reminder>
// XML tags telling the LLM "this is system-level context, not user input".
func buildLoopTaskPrompt(loop *model.Loop) string {
	return fmt.Sprintf(`<system-reminder>
A scheduled loop task is now due for execution.

Loop ID: %s
Progress: Turn %d of %d
Task: Execute the loop's recurring task for THIS TURN ONLY (Turn %d).

IMPORTANT:
- Execute the loop prompt for this turn only
- Do NOT execute subsequent turns proactively
- After completing this turn, STOP and wait for the next system-reminder
- Each turn is triggered separately by the system

Please proceed with this turn of the loop task.
</system-reminder>`,
		loop.ID.String(),
		loop.CompletedTurns+1,
		loop.MaxTurns,
		loop.CompletedTurns+1,
	)
}

// RegisterHandlers registers asynq handlers.
func (w *Worker) RegisterHandlers(mux *hibikenasynq.ServeMux) {
	mux.HandleFunc(LoopTaskType, w.HandleLoopTask)
}

// StartAsynqServer starts the asynq server.
func StartAsynqServer(redisAddr string, worker *Worker) error {
	srv := hibikenasynq.NewServer(
		hibikenasynq.RedisClientOpt{Addr: redisAddr},
		hibikenasynq.Config{
			Concurrency: 10,
			Queues: map[string]int{
				"loop":    6,
				"default": 3,
			},
		},
	)

	mux := hibikenasynq.NewServeMux()
	worker.RegisterHandlers(mux)

	return srv.Run(mux)
}
