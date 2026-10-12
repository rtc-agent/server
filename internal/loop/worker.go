// Package loop implements the Loop command's background task processing.
//
// Worker handles asynq tasks for loop execution, Recovery monitors for stale
// or expired loops, and Cleanup handles session close cleanup.
//
// Prometheus metrics are defined in metrics.go and cover:
//   - loop_worker_tasks_total{status="success|failure"}: Counter of processed tasks
//   - loop_worker_task_duration_seconds: Histogram of task processing duration
//   - loop_recovery_scans_total: Counter of recovery scans
//   - loop_recovery_attempts_total{status="success|failure|exhausted"}: Counter of recovery attempts
//
// Concurrency model:
//
//	asynq guarantees single-node consumption through Redis BRPOPLPUSH semantics.
//	A task can only be in "processing" state on one worker at a time. Combined with
//	TaskID-based idempotency (loop.ID), this ensures exactly-once processing without
//	the need for distributed locks.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

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
}

// NewWorker creates a loop worker.
// Note: asynq provides single-node consumption guarantee, so no Redis client
// is needed for distributed locking. TaskID-based idempotency prevents duplicate processing.
func NewWorker(queue *rtcqueue.Queue, loopRepo usecase.LoopRepo, sessionRepo usecase.SessionRepo, notificationCreator NotificationCreator) *Worker {
	return &Worker{
		queue:               queue,
		loopRepo:            loopRepo,
		sessionRepo:         sessionRepo,
		notificationCreator: notificationCreator,
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
// Concurrency safety: asynq guarantees single-node consumption through Redis
// BRPOPLPUSH semantics. A task in "processing" state cannot be picked up by
// another worker. Combined with TaskID-based idempotency (using loop.ID), this
// ensures exactly-once processing without distributed locks.
//
// Following the async sub-agent pattern:
//  1. Validate loop is still active
//  2. Check idempotency via LastRunAt (secondary safeguard)
//  3. Update LastRunAt (prevents duplicate triggers if publish fails)
//  4. Build the loop task prompt
//  5. Create a user-role notification message in the session
//  6. Publish WorkKindSubmit to trigger a new turn
//  7. The LLM sees the notification as the most recent instruction
//
// Error handling: On failure, logs detailed context for debugging and traceability.
// Failed tasks are archived by asynq and can be inspected via asynqmon or manually retried.
func (w *Worker) HandleLoopTask(ctx context.Context, t *hibikenasynq.Task) error {
	startTime := time.Now()

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

	// Validate loop state
	// Note: No distributed lock needed - asynq guarantees single-node consumption
	loop, err := w.validateLoopState(ctx, sessionUUID, loopUUID, span)
	if err != nil {
		recordTaskFailure(time.Since(startTime))
		return err
	}
	if loop == nil {
		return nil
	}

	// Idempotency check
	alreadyProcessed := w.checkIdempotency(ctx, loop, sessionUUID, loopUUID, span)
	if alreadyProcessed {
		recordTestIdempotentSkip(time.Since(startTime))
		return nil
	}

	// Fix(P0-1): Update LastRunAt BEFORE publishing to rtc-queue.
	// Rationale: If we publish first and then fail to update LastRunAt,
	// the task is already queued for LLM execution but idempotency check
	// would think it wasn't processed — leading to duplicate triggers.
	//
	// Accepting the inverse risk (LastRunAt updated but publish fails) is safer:
	// the recovery mechanism will detect stale loops (missing asynq task within
	// staleLoopThreshold) and re-enqueue them on the next scan.
	//
	// Fix(P1-4): Reset recovery_attempts atomically with last_run_at to prevent
	// accumulation on successful task processing. If these were separate updates
	// and the second failed, recovery_attempts could accumulate and cause premature
	// exhaustion marking.
	now := time.Now()
	updateFields := map[string]any{"last_run_at": now}
	if loop.RecoveryAttempts > 0 {
		updateFields["recovery_attempts"] = 0
	}
	if err := w.loopRepo.Update(ctx, loopUUID, updateFields); err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		logTaskFailure(ctx, "update loop state", sessionUUID.String(), loopUUID.String(), err)
		recordTaskFailure(time.Since(startTime))
		return fmt.Errorf("update loop state: %w", err)
	}

	// Build prompt and create notification
	prompt := buildLoopTaskPrompt(loop)
	if w.notificationCreator != nil {
		if err := w.notificationCreator(ctx, sessionUUID, prompt); err != nil {
			span.SetStatus(codes.Error, "internal_error")
			span.RecordError(err)
			logTaskFailure(ctx, "create notification message", sessionUUID.String(), loopUUID.String(), err)
			recordTaskFailure(time.Since(startTime))
			return fmt.Errorf("create notification message: %w", err)
		}
	}

	// Get userID from session (optional, for observability)
	userID := w.getUserIDFromSession(ctx, sessionUUID)

	// Publish to rtc-queue with deterministic work ID for idempotency.
	// Use loop ID and turn number to ensure retries produce the same work ID.
	if err := w.publishToQueue(ctx, sessionUUID.String(), userID, traceID, spanID, loop.ID.String(), loop.CompletedTurns+1, span); err != nil {
		logTaskFailure(ctx, "publish to rtc-queue", sessionUUID.String(), loopUUID.String(), err)
		recordTaskFailure(time.Since(startTime))
		return err
	}

	recordTaskSuccess(time.Since(startTime))

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

// publishToQueue publishes the work item to the rtc-queue with deterministic work ID.
func (w *Worker) publishToQueue(ctx context.Context, sessionID, userID, traceID, spanID, loopID string, turnNumber int, span trace.Span) error {
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

	// Generate deterministic work ID for idempotency.
	// Format: "loop:{loopID}:turn:{turnNumber}" ensures retries produce the same ID.
	workID := rtcqueue.GenerateDeterministicWorkID(sessionID, fmt.Sprintf("loop:%s:turn:%d", loopID, turnNumber))

	_, err = w.queue.PublishWithID(ctx, sessionID, workID, string(workPayload), rtcqueue.SubmitWorkPriority)
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
		// Security: Avoid logging raw payload to prevent sensitive data exposure.
		// The payload structure is well-defined and should not contain secrets,
		// but defensive logging prevents accidental leaks if the schema changes.
		logger.Error(ctx, "[loop.Worker] unmarshal payload",
			zap.Error(err))
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
