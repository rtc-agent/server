package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	hibikenasynq "github.com/hibiken/asynq"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/dbutil"
	looppkg "github.com/rtc-agent/server/internal/loop"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// LoopWorkflow is the /loop command implementation, wired into the
// slash-command framework. It mirrors GoalWorkflow but for recurring tasks.
//
// Unlike GoalWorkflow which re-queues immediately via rtc-queue,
// LoopWorkflow schedules delayed tasks via TaskScheduler (asynq in batch 3).
type LoopWorkflow struct {
	helpers  *helpers
	registry *command.CommandRegistry
}

// Name returns the command name "loop".
func (l *LoopWorkflow) Name() string { return "loop" }

// Prefix returns the slash prefix "/loop".
func (l *LoopWorkflow) Prefix() string { return "/loop" }

// Scope returns ScopeSession, keeping the loop command active for the session.
func (l *LoopWorkflow) Scope() command.Scope { return command.ScopeSession }

// PromptPersistConfig declares that TriggerPrompt should be persisted as a prompt message.
// The prompt is stored with name="command", title="loop", role="user" so it will be
// merged with consecutive user messages by normalizeMessagesForLLM.
func (l *LoopWorkflow) PromptPersistConfig() command.PromptPersistConfig {
	return command.PromptPersistConfig{
		Persist: true,
		Name:    "command",
		Title:   "loop",
	}
}

// TriggerPrompt returns the loop creation user prompt. Called on the turn
// the user types "/loop ...".
func (l *LoopWorkflow) TriggerPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	return &command.PromptContribution{
		Role:    "user",
		Content: loopCreationPrompt,
	}, nil
}

// SustainPrompt returns nil because the loop worker's notification message
// already provides all necessary context (progress, management instructions).
// Returning nil avoids redundant injection and prevents polluting the system
// array with dynamic content that would invalidate the cache prefix.
//
// This aligns with the design principle: trigger messages (notifications)
// should carry all context needed for the turn, making SustainPrompt redundant.
func (l *LoopWorkflow) SustainPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	return nil, nil
}

// Tools returns the loop tool set for the current turn. The tools need the
// session and turnID, which are read from command.Context on each call.
func (l *LoopWorkflow) Tools(ctx command.Context) []tool.BaseTool {
	session, err := l.helpers.deps.SessionRepo.GetByID(ctx, ctx.SessionID)
	if err != nil || session == nil {
		return nil
	}
	return []tool.BaseTool{
		&createLoopTool{session: session, helpers: l.helpers, turnID: ctx.TurnID},
		&pauseLoopTool{session: session, helpers: l.helpers, turnID: ctx.TurnID},
		&resumeLoopTool{session: session, helpers: l.helpers, turnID: ctx.TurnID},
		&cancelLoopTool{session: session, helpers: l.helpers, turnID: ctx.TurnID},
		&completeLoopTool{session: session, helpers: l.helpers, turnID: ctx.TurnID},
		&listLoopsTool{session: session, helpers: l.helpers, turnID: ctx.TurnID},
	}
}

// OnTurnComplete runs the loop execution loop:
//  1. Find active loop for the session (with row lock to prevent race)
//  2. Increment completed_turns atomically within transaction
//  3. If max_turns exceeded → mark exhausted (no re-schedule)
//  4. Else → persist count and schedule next delayed task via TaskScheduler
//
// The loop command stays activated for the session's lifetime regardless of
// loop state. SustainPrompt returns nil when no active loop exists (either
// during the confirmation phase before createLoop, or after the loop
// reaches a terminal state), making the command inert in those turns.
//
// IMPORTANT: this hook must NOT call registry methods (Deactivate, etc.)
// because the registry holds its mutex while invoking hooks — doing so
// would deadlock.
//
// CONCURRENCY: Uses SELECT FOR UPDATE to prevent race conditions when two
// turns complete nearly simultaneously. Without the lock, both could read
// the same completed_turns value, increment it, and both schedule the next
// task — causing duplicate execution.
func (l *LoopWorkflow) OnTurnComplete(ctx command.Context) error {
	innerCtx, span := l.helpers.tracer.Start(ctx.Context, "loop_workflow.on_turn_complete",
		trace.WithAttributes(
			attribute.String("session.id", ctx.SessionID.String()),
			attribute.String("turn.id", ctx.TurnID.String()),
		),
	)
	defer span.End()
	ctx.Context = innerCtx

	if l.helpers.deps.LoopRepo == nil {
		return nil
	}

	// Use a transaction with SELECT FOR UPDATE to prevent race conditions.
	// Two concurrent turn completions could otherwise read the same completed_turns
	// value, both increment it, and both schedule the next task.
	var loop *model.Loop
	var newTurns int
	err := l.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
		txCtx := dbutil.WithTx(ctx, tx)

		// Lock the row to prevent concurrent updates.
		// SELECT FOR UPDATE ensures that if two transactions try to lock the same
		// row, one will wait for the other to complete.
		var lockedLoop model.Loop
		result := tx.WithContext(txCtx).
			Where("session_id = ? AND status = ?", ctx.SessionID, model.LoopStatusActive).
			Order("created_at DESC").
			First(&lockedLoop)

		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				// No active loop: either we're in the confirmation phase before
				// createLoop was called, or the loop was just completed/cancelled
				// by the LLM during this turn. Nothing to do — command stays
				// activated but inert.
				span.SetAttributes(attribute.Bool("loop.active", false))
				return nil
			}
			return fmt.Errorf("find active loop for update: %w", result.Error)
		}

		loop = &lockedLoop
		newTurns = loop.CompletedTurns + 1
		span.SetAttributes(
			attribute.String("loop.id", loop.ID.String()),
			attribute.Int("loop.max_turns", loop.MaxTurns),
			attribute.Int("loop.completed_turns", newTurns),
		)

		if newTurns > loop.MaxTurns {
			span.SetAttributes(attribute.String("loop.status", "exhausted"))
			return l.helpers.deps.LoopRepo.Update(txCtx, loop.ID, map[string]any{
				"status":          model.LoopStatusExhausted,
				"completed_turns": newTurns,
			})
		}

		// Update completed turns.
		// Note: asynq_task_id is not maintained here because we use loop.ID as the task ID
		// for idempotent scheduling (set in scheduleNextLoop via TaskID option).
		// This simplifies state management and avoids race conditions.
		return l.helpers.deps.LoopRepo.Update(txCtx, loop.ID, map[string]any{
			"completed_turns": newTurns,
			"last_run_at":     time.Now(),
		})
	})

	if err != nil {
		if loop == nil {
			// Transaction returned nil (no active loop found) — not an error.
			return nil
		}
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		l.helpers.logger.Warn(ctx, "loopWorkflow.transaction_failed", map[string]any{
			"loop_id":    loop.ID.String(),
			"session_id": ctx.SessionID.String(),
			"error":      err.Error(),
		})
		return err
	}

	if loop == nil {
		// No active loop found — nothing to do.
		return nil
	}

	if newTurns > loop.MaxTurns {
		l.helpers.logger.Info(ctx, "loopWorkflow.loop_exhausted", map[string]any{
			"loop_id":         loop.ID.String(),
			"session_id":      ctx.SessionID.String(),
			"completed_turns": newTurns,
			"max_turns":       loop.MaxTurns,
		})
		// No re-schedule — loop has ended.
		return nil
	}

	// Schedule next delayed task via TaskScheduler (fire-and-forget).
	// TaskScheduler may be nil in batch 2 (implementation in batch 3).
	//
	// IMPORTANT: use context.WithoutCancel(ctx) — preserves trace values while
	// stripping the cancel signal, ensuring the scheduling survives request teardown.
	// The request context may be cancelled when OnTurnComplete returns (the registry
	// holds its write lock during hook invocation). A detached context ensures the
	// scheduling survives request teardown. Panic recovery is required because this
	// goroutine is outside any recover boundary.
	span.SetAttributes(attribute.String("loop.status", "extended"))
	if l.helpers.deps.TaskScheduler != nil {
		// Capture detached context before entering goroutine (preserves trace information)
		detachedCtx := context.WithoutCancel(ctx)
		logger.SafeGo("loopWorkflow.scheduleNext", func() {
			// Use a context with timeout to prevent asynq calls from hanging and causing goroutine leaks.
			schedCtx, schedCancel := context.WithTimeout(detachedCtx, 10*time.Second)
			defer schedCancel()
			l.scheduleNextLoop(schedCtx, loop)
		})
	}

	l.helpers.logger.Info(ctx, "loopWorkflow.loop_extended", map[string]any{
		"loop_id":         loop.ID.String(),
		"session_id":      ctx.SessionID.String(),
		"completed_turns": newTurns,
		"max_turns":       loop.MaxTurns,
	})
	return nil
}

// loopSchedulePayload is the JSON payload that scheduleNextLoop writes to TaskScheduler.
// Uses a struct instead of map[string]any to avoid runtime reflection for field lookup and gain type safety.
type loopSchedulePayload struct {
	LoopID    string `json:"loop_id"`
	SessionID string `json:"session_id"`

	// TraceID is the OpenTelemetry trace ID from the request that scheduled this loop task.
	// Used to propagate trace context across process boundaries (asynq → rtcqueue).
	// Empty for legacy payloads or when no trace context is available.
	TraceID string `json:"trace_id,omitempty"`

	// SpanID is the OpenTelemetry span ID from the request that scheduled this loop task.
	// Paired with TraceID to restore the full span context on the worker side.
	// Empty for legacy payloads or when no trace context is available.
	SpanID string `json:"span_id,omitempty"`
}

// scheduleNextLoop schedules the next loop turn via TaskScheduler.
// This runs in a separate goroutine (fire-and-forget) to avoid blocking
// the turn completion. The caller passes a context derived from context.WithoutCancel(ctx)
// (not the request context) because this goroutine outlives the request, but we preserve
// trace values for observability. Errors are logged but not propagated.
//
// Idempotency: Uses loop.ID as the asynq task ID. If a task with this ID already exists
// (pending or processing), asynq returns ErrTaskIDConflict, making repeated scheduling safe.
// This eliminates the need to maintain a separate asynq_task_id field in the database.
func (l *LoopWorkflow) scheduleNextLoop(ctx context.Context, loop *model.Loop) {
	ctx, span := l.helpers.tracer.Start(ctx, "loop_workflow.schedule_next",
		trace.WithAttributes(
			attribute.String("loop.id", loop.ID.String()),
			attribute.String("session.id", loop.SessionID.String()),
			attribute.Int("loop.interval_seconds", loop.IntervalSeconds),
		),
	)
	defer span.End()

	// Extract trace context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)

	payload, marshalErr := json.Marshal(&loopSchedulePayload{
		LoopID:    loop.ID.String(),
		SessionID: loop.SessionID.String(),
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if marshalErr != nil {
		span.SetStatus(codes.Error, marshalErr.Error())
		span.RecordError(marshalErr)
		l.helpers.logger.Warn(ctx, "loopWorkflow.marshal_failed", map[string]any{
			"loop_id": loop.ID.String(),
			"error":   marshalErr.Error(),
		})
		return
	}

	delay := time.Duration(loop.IntervalSeconds) * time.Second

	// Use loop ID as task ID for idempotency.
	// If a task with this ID already exists (pending/processing), asynq returns
	// ErrTaskIDConflict, making repeated scheduling safe (e.g., from recovery).
	taskID := loop.ID.String()

	scheduledTaskID, err := l.helpers.deps.TaskScheduler.ScheduleDelayed(ctx, looppkg.LoopTaskType, payload, delay, taskID)
	if err != nil {
		// Check if task already exists (idempotent)
		if errors.Is(err, hibikenasynq.ErrTaskIDConflict) {
			span.SetAttributes(attribute.String("task.idempotent", "true"))
			l.helpers.logger.Info(ctx, "loopWorkflow.task_already_exists", map[string]any{
				"loop_id": loop.ID.String(),
				"task_id": taskID,
			})
			return // Idempotent: task already scheduled
		}
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		l.helpers.logger.Warn(ctx, "loopWorkflow.schedule_failed", map[string]any{
			"loop_id":    loop.ID.String(),
			"session_id": loop.SessionID.String(),
			"error":      err.Error(),
		})
		return
	}
	span.SetAttributes(
		attribute.String("task.id", scheduledTaskID),
		attribute.String("task.delay", delay.String()),
	)

	// Note: No need to update loop.asynq_task_id since we use loop.ID as the task ID.
	// The task ID is implicit and always equals loop.ID when scheduled.
	// This simplifies state management and avoids race conditions.

	l.helpers.logger.Info(ctx, "loopWorkflow.scheduled_next", map[string]any{
		"loop_id":    loop.ID.String(),
		"session_id": loop.SessionID.String(),
		"task_id":    taskID,
		"delay":      delay.String(),
	})
}

// registerLoopCommand adds the /loop workflow to the registry. Called from
// agent.New after helpers is constructed, so the workflow can close over it.
func registerLoopCommand(registry *command.CommandRegistry, h *helpers) {
	if registry == nil {
		return
	}
	registry.Register(&LoopWorkflow{helpers: h, registry: registry})
}
