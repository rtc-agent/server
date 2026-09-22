// Package loop implements the Loop command's background task processing.
//
// Worker handles asynq tasks for loop execution, Recovery monitors for stale
// or expired loops, and Cleanup handles session close cleanup.
package loop

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	hibikenasynq "github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
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
	loopRepo            repo.LoopRepo
	notificationCreator NotificationCreator
}

// NewWorker creates a loop worker.
func NewWorker(queue *rtcqueue.Queue, loopRepo repo.LoopRepo, notificationCreator NotificationCreator) *Worker {
	return &Worker{
		queue:               queue,
		loopRepo:            loopRepo,
		notificationCreator: notificationCreator,
	}
}

// HandleLoopTask processes loop:execute tasks.
//
// Idempotency: safe to call multiple times. If the loop no longer exists,
// is not active, or the ID doesn't match, returns nil without side effects.
//
// Following the async sub-agent pattern:
//  1. Validate loop is still active
//  2. Build the loop task prompt
//  3. Create a user-role notification message in the session (so it becomes
//     the last user message in conversation history)
//  4. Publish WorkKindSubmit to trigger a new turn
//  5. The LLM sees the notification as the most recent instruction
func (w *Worker) HandleLoopTask(ctx context.Context, t *hibikenasynq.Task) error {
	var payload struct {
		SessionID string `json:"session_id"`
		LoopID    string `json:"loop_id"`
		TraceID   string `json:"trace_id,omitempty"`
		SpanID    string `json:"span_id,omitempty"`
	}

	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Error(ctx, "[loop.Worker] unmarshal payload",
			zap.Error(err))
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	// Restore trace context from payload (if available).
	// This preserves the original request's trace information across the asynq boundary.
	if payload.TraceID != "" && payload.SpanID != "" {
		ctx = turnagent.WithTraceContext(ctx, payload.TraceID, payload.SpanID)
	}

	ctx, span := workerTracer().Start(ctx, "loop.HandleLoopTask",
		trace.WithAttributes(
			attribute.String("session.id", payload.SessionID),
			attribute.String("loop.id", payload.LoopID),
		),
	)
	defer span.End()

	sessionUUID, err := uuid.Parse(payload.SessionID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[loop.Worker] parse session ID",
			zap.String("session_id", payload.SessionID),
			zap.Error(err))
		return fmt.Errorf("parse session ID: %w", err)
	}
	loopUUID, err := uuid.Parse(payload.LoopID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[loop.Worker] parse loop ID",
			zap.String("loop_id", payload.LoopID),
			zap.Error(err))
		return fmt.Errorf("parse loop ID: %w", err)
	}

	// 1. Validate loop still exists and is active
	loop, err := w.loopRepo.FindActive(ctx, sessionUUID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[loop.Worker] find active loop",
			zap.String("session_id", sessionUUID.String()),
			zap.Error(err))
		return fmt.Errorf("find active loop: %w", err)
	}
	if loop == nil {
		// Loop was cancelled/paused, no longer executing
		span.SetAttributes(attribute.String("outcome", "loop_not_found"))
		logger.Debug(ctx, "[loop.Worker] loop no longer active, skipping",
			zap.String("session_id", sessionUUID.String()),
			zap.String("loop_id", loopUUID.String()))
		return nil
	}
	if loop.ID != loopUUID {
		// Loop ID doesn't match (may have been replaced by a new loop)
		span.SetAttributes(attribute.String("outcome", "loop_id_mismatch"))
		logger.Debug(ctx, "[loop.Worker] loop ID mismatch, skipping",
			zap.String("session_id", sessionUUID.String()),
			zap.String("expected_loop_id", loopUUID.String()),
			zap.String("active_loop_id", loop.ID.String()))
		return nil
	}

	// 2. Build the loop task prompt
	prompt := buildLoopTaskPrompt(loop)

	// 3. Create user-role notification message (async sub-agent pattern)
	if w.notificationCreator != nil {
		if err := w.notificationCreator(ctx, sessionUUID, prompt); err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			logger.Error(ctx, "[loop.Worker] create notification message",
				zap.String("session_id", sessionUUID.String()),
				zap.String("loop_id", loop.ID.String()),
				zap.Error(err))
			return fmt.Errorf("create notification message: %w", err)
		}
	}

	// 4. Submit to rtc-queue for execution (only SessionID, consistent with Goal pattern)
	// Pass trace_id to maintain trace context across the rtcqueue boundary.
	workPayload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: payload.SessionID,
		TraceID:   payload.TraceID,
		SpanID:    payload.SpanID,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[loop.Worker] marshal work payload",
			zap.String("session_id", sessionUUID.String()),
			zap.Error(err))
		return fmt.Errorf("marshal work payload: %w", err)
	}

	_, err = w.queue.Publish(ctx, payload.SessionID, string(workPayload), rtcqueue.SubmitWorkPriority)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Error(ctx, "[loop.Worker] publish to rtc-queue",
			zap.String("session_id", sessionUUID.String()),
			zap.String("loop_id", loop.ID.String()),
			zap.Error(err))
		return fmt.Errorf("publish to rtc-queue: %w", err)
	}

	span.SetAttributes(attribute.Int("loop.turn", loop.CompletedTurns+1))
	logger.Info(ctx, "[loop.Worker] task processed successfully",
		zap.String("session_id", sessionUUID.String()),
		zap.String("loop_id", loop.ID.String()),
		zap.Int("turn", loop.CompletedTurns+1),
		zap.Int("max_turns", loop.MaxTurns))

	return nil
}

// buildLoopTaskPrompt constructs the notification text for a loop task trigger.
// Following the async sub-agent pattern: user-role message with <system-reminder>
// XML tags telling the LLM "this is system-level context, not user input".
func buildLoopTaskPrompt(loop *model.Loop) string {
	return fmt.Sprintf(`<system-reminder>
A scheduled loop task is now due for execution.

Loop ID: %s
Progress: Turn %d of %d
Task: Continue executing the loop's recurring task as defined when the loop was created.

Please proceed with the next iteration of the loop task.
</system-reminder>`,
		loop.ID.String(),
		loop.CompletedTurns+1,
		loop.MaxTurns,
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
