package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
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

func (l *LoopWorkflow) Name() string         { return "loop" }
func (l *LoopWorkflow) Prefix() string       { return "/loop" }
func (l *LoopWorkflow) Scope() command.Scope { return command.ScopeSession }

// TriggerPrompt returns the loop creation user prompt. Called on the turn
// the user types "/loop ...".
func (l *LoopWorkflow) TriggerPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	return &command.PromptContribution{
		Role:    "system",
		Content: loopCreationPrompt,
	}, nil
}

// SustainPrompt returns the loop management user prompt when there is an
// active loop for the session. Returns nil when no loop is active or the
// loop has reached a terminal state.
func (l *LoopWorkflow) SustainPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	if l.helpers.deps.LoopRepo == nil {
		return nil, nil
	}
	loop, err := l.helpers.deps.LoopRepo.FindActive(ctx, ctx.SessionID)
	if err != nil || loop == nil {
		return nil, nil
	}
	return &command.PromptContribution{
		Role:    "system",
		Content: buildLoopManagementPrompt(loop),
	}, nil
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
//  1. Find active loop for the session
//  2. Increment completed_turns
//  3. If max_turns exceeded → mark exhausted (no re-schedule)
//  4. Else → persist count and schedule next delayed task via TaskScheduler
//
// The loop command stays activated for the session's lifetime regardless of
// loop state. SustainPrompt returns nil when no active loop exists (either
// during the confirmation phase before create_loop, or after the loop
// reaches a terminal state), making the command inert in those turns.
//
// IMPORTANT: this hook must NOT call registry methods (Deactivate, etc.)
// because the registry holds its mutex while invoking hooks — doing so
// would deadlock.
func (l *LoopWorkflow) OnTurnComplete(ctx command.Context) error {
	if l.helpers.deps.LoopRepo == nil {
		return nil
	}

	loop, err := l.helpers.deps.LoopRepo.FindActive(ctx, ctx.SessionID)
	if err != nil {
		l.helpers.logIfEnabled(ctx, "loopWorkflow.find_active_failed", map[string]any{
			"session_id": ctx.SessionID.String(),
			"error":      err.Error(),
		})
		return nil // degrade
	}
	if loop == nil {
		// No active loop: either we're in the confirmation phase before
		// create_loop was called, or the loop was just completed/cancelled
		// by the LLM during this turn. Nothing to do — command stays
		// activated but inert.
		return nil
	}

	newTurns := loop.CompletedTurns + 1

	if newTurns > loop.MaxTurns {
		err = l.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
			// Inject transaction into context so LoopRepo.Update uses it
			txCtx := repo.WithTx(ctx, tx)
			return l.helpers.deps.LoopRepo.Update(txCtx, loop.ID, map[string]any{
				"status":          string(model.LoopStatusExhausted),
				"completed_turns": newTurns,
			})
		})
		if err != nil {
			l.helpers.logIfEnabled(ctx, "loopWorkflow.update_exhausted_failed", map[string]any{
				"loop_id": loop.ID.String(),
				"error":   err.Error(),
			})
			return err
		}
		l.helpers.logIfEnabled(ctx, "loopWorkflow.loop_exhausted", map[string]any{
			"loop_id":         loop.ID.String(),
			"session_id":      ctx.SessionID.String(),
			"completed_turns": newTurns,
			"max_turns":       loop.MaxTurns,
		})
		// No re-schedule — loop has ended.
		return nil
	}

	// Update completed turns and clear asynq_task_id atomically.
	// The asynq_task_id is cleared before scheduling the next task to avoid
	// stale references. The new task ID will be set by the scheduler.
	err = l.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
		// Inject transaction into context so LoopRepo.Update uses it
		txCtx := repo.WithTx(ctx, tx)
		return l.helpers.deps.LoopRepo.Update(txCtx, loop.ID, map[string]any{
			"completed_turns": newTurns,
			"asynq_task_id":   "",
			"last_run_at":     time.Now(),
		})
	})
	if err != nil {
		l.helpers.logIfEnabled(ctx, "loopWorkflow.update_loop_failed", map[string]any{
			"loop_id": loop.ID.String(),
			"error":   err.Error(),
		})
		return err
	}

	// Schedule next delayed task via TaskScheduler (fire-and-forget).
	// TaskScheduler may be nil in batch 2 (implementation in batch 3).
	//
	// IMPORTANT: use context.Background() — the request context may be
	// cancelled when OnTurnComplete returns (the registry holds its write
	// lock during hook invocation). A detached context ensures the
	// scheduling survives request teardown. Panic recovery is required
	// because this goroutine is outside any recover boundary.
	if l.helpers.deps.TaskScheduler != nil {
		loopID := loop.ID
		sessionID := ctx.SessionID
		go func() {
			defer func() {
				if r := recover(); r != nil {
					l.helpers.logIfEnabled(context.Background(), "loopWorkflow.schedule_panic", map[string]any{
						"loop_id":    loopID.String(),
						"session_id": sessionID.String(),
						"panic":      fmt.Sprintf("%v", r),
					})
				}
			}()
			l.scheduleNextLoop(context.Background(), loop)
		}()
	}

	l.helpers.logIfEnabled(ctx, "loopWorkflow.loop_extended", map[string]any{
		"loop_id":         loop.ID.String(),
		"session_id":      ctx.SessionID.String(),
		"completed_turns": newTurns,
		"max_turns":       loop.MaxTurns,
	})
	return nil
}

// scheduleNextLoop schedules the next loop turn via TaskScheduler.
// This runs in a separate goroutine (fire-and-forget) to avoid blocking
// the turn completion. The caller MUST pass context.Background() (not the
// request context) because this goroutine outlives the request. Errors
// are logged but not propagated.
func (l *LoopWorkflow) scheduleNextLoop(ctx context.Context, loop *model.Loop) {
	payload, marshalErr := json.Marshal(map[string]any{
		"loop_id":    loop.ID.String(),
		"session_id": loop.SessionID.String(),
	})
	if marshalErr != nil {
		l.helpers.logIfEnabled(ctx, "loopWorkflow.marshal_failed", map[string]any{
			"loop_id": loop.ID.String(),
			"error":   marshalErr.Error(),
		})
		return
	}

	delay := time.Duration(loop.IntervalSeconds) * time.Second
	taskID, err := l.helpers.deps.TaskScheduler.ScheduleDelayed(ctx, "loop_turn", payload, delay)
	if err != nil {
		l.helpers.logIfEnabled(ctx, "loopWorkflow.schedule_failed", map[string]any{
			"loop_id":    loop.ID.String(),
			"session_id": loop.SessionID.String(),
			"error":      err.Error(),
		})
		return
	}

	// Update loop with the new task ID.
	if updateErr := l.helpers.deps.LoopRepo.Update(ctx, loop.ID, map[string]any{
		"asynq_task_id": taskID,
	}); updateErr != nil {
		l.helpers.logIfEnabled(ctx, "loopWorkflow.update_task_id_failed", map[string]any{
			"loop_id": loop.ID.String(),
			"error":   updateErr.Error(),
		})
	}

	l.helpers.logIfEnabled(ctx, "loopWorkflow.scheduled_next", map[string]any{
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
