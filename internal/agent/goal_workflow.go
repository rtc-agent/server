package agent

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/model"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// GoalWorkflow is the /goal command implementation, wired into the
// slash-command framework. It replaces the legacy hard-coded wiring:
//
//   - injectGoalCreationPromptIfNeeded   → TriggerPrompt
//   - injectGoalManagementPromptIfNeeded → SustainPrompt
//   - createGoal / completeGoal / cancelGoal tool registration → Tools
//   - handleGoalOnTurnComplete           → OnTurnComplete
type GoalWorkflow struct {
	helpers  *helpers
	registry *command.CommandRegistry
}

// Name returns the command name "goal".
func (g *GoalWorkflow) Name() string { return "goal" }

// Prefix returns the slash prefix "/goal".
func (g *GoalWorkflow) Prefix() string { return "/goal" }

// Scope returns ScopeSession, keeping the goal command active for the session.
func (g *GoalWorkflow) Scope() command.Scope { return command.ScopeSession }

// PromptPersistConfig declares that TriggerPrompt should be persisted as a prompt message.
// The prompt is stored with name="command", title="goal", role="user" so it will be
// merged with consecutive user messages by normalizeMessagesForLLM.
func (g *GoalWorkflow) PromptPersistConfig() command.PromptPersistConfig {
	return command.PromptPersistConfig{
		Persist: true,
		Name:    "command",
		Title:   "goal",
	}
}

// TriggerPrompt returns the goal creation user prompt. Called on the turn
// the user types "/goal ...".
func (g *GoalWorkflow) TriggerPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	return &command.PromptContribution{
		Role:    "user",
		Content: goalCreationPrompt,
	}, nil
}

// SustainPrompt returns the goal management user prompt when there is an
// active goal for the session. Returns nil when no goal is active or the
// goal has reached a terminal state.
//
// IMPORTANT: Role is "user" (not "system") so the prompt is appended to the
// end of the message array. This ensures the LLM sees the goal task as the
// most recent instruction.
// SustainPrompt returns nil because the turn trigger (rtc-queue re-queue or
// notification) already provides all necessary context about the goal state.
// Returning nil avoids redundant injection and simplifies the message pipeline.
//
// This aligns with the design principle: trigger messages should carry all
// context needed for the turn, making SustainPrompt redundant.
func (g *GoalWorkflow) SustainPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	return nil, nil
}

// Tools returns the goal tool set for the current turn. The tools need the
// session and turnID, which are read from command.Context on each call.
func (g *GoalWorkflow) Tools(ctx command.Context) []tool.BaseTool {
	session, err := g.helpers.deps.SessionRepo.GetByID(ctx, ctx.SessionID)
	if err != nil || session == nil {
		return nil
	}
	return []tool.BaseTool{
		&createGoalTool{session: session, helpers: g.helpers, turnID: ctx.TurnID},
		&completeGoalTool{session: session, helpers: g.helpers, turnID: ctx.TurnID},
		&cancelGoalTool{session: session, helpers: g.helpers, turnID: ctx.TurnID},
	}
}

// OnTurnComplete runs the goal execution loop:
//  1. Find active goal for the session
//  2. Increment completed_turns
//  3. If max_turns exceeded → mark exhausted (no re-queue)
//  4. Else → persist count and re-queue a submit work item
//
// The goal command stays activated for the session's lifetime regardless of
// goal state. SustainPrompt returns nil when no active goal exists (either
// during the confirmation phase before createGoal, or after the goal
// reaches a terminal state), making the command inert in those turns. This
// matches the legacy behavior: goal tools were always registered, and
// management prompts were only injected when an active goal existed.
//
// IMPORTANT: this hook must NOT call registry methods (Deactivate, etc.)
// because the registry holds its mutex while invoking hooks — doing so
// would deadlock.
func (g *GoalWorkflow) OnTurnComplete(ctx command.Context) error {
	innerCtx, span := g.helpers.tracer.Start(ctx.Context, "goalWorkflow.onTurnComplete",
		trace.WithAttributes(
			attribute.String("session.id", ctx.SessionID.String()),
			attribute.String("turn.id", ctx.TurnID.String()),
		),
	)
	defer span.End()
	ctx.Context = innerCtx

	if g.helpers.deps.GoalRepo == nil {
		return nil
	}

	goal, err := g.helpers.deps.GoalRepo.FindActive(ctx, ctx.SessionID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		g.helpers.logger.Warn(ctx, "goalWorkflow.find_active_failed", map[string]any{
			"session_id": ctx.SessionID.String(),
			"error":      err.Error(),
		})
		return nil // degrade
	}
	if goal == nil {
		// No active goal: either we're in the confirmation phase before
		// createGoal was called, or the goal was just completed/cancelled
		// by the LLM during this turn. Nothing to do — command stays
		// activated but inert.
		span.SetAttributes(attribute.Bool("goal.active", false))
		return nil
	}
	span.SetAttributes(
		attribute.String("goal.id", goal.ID.String()),
		attribute.Int("goal.max_turns", goal.MaxTurns),
	)

	newTurns := goal.CompletedTurns + 1
	span.SetAttributes(attribute.Int("goal.completed_turns", newTurns))

	if newTurns > goal.MaxTurns {
		span.SetAttributes(attribute.String("goal.status", "exhausted"))
		err = g.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
			return g.helpers.deps.GoalRepo.Update(ctx, goal.ID, map[string]any{
				"status":          model.GoalStatusExhausted,
				"completed_turns": newTurns,
			})
		})
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			g.helpers.logger.Warn(ctx, "goalWorkflow.update_exhausted_failed", map[string]any{
				"goal_id": goal.ID.String(),
				"error":   err.Error(),
			})
			return err
		}
		g.helpers.logger.Info(ctx, "goalWorkflow.goal_exhausted", map[string]any{
			"goal_id":         goal.ID.String(),
			"session_id":      ctx.SessionID.String(),
			"completed_turns": newTurns,
			"max_turns":       goal.MaxTurns,
		})
		// No re-queue — goal has ended.
		return nil
	}

	err = g.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
		return g.helpers.deps.GoalRepo.Update(ctx, goal.ID, map[string]any{
			"completed_turns": newTurns,
		})
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		g.helpers.logger.Warn(ctx, "goalWorkflow.update_goal_failed", map[string]any{
			"goal_id": goal.ID.String(),
			"error":   err.Error(),
		})
		return err
	}

	// Create user-role notification message before triggering next turn.
	// This follows the async sub-agent pattern (same as loop workflow):
	// - Notification becomes the new "last user message" in conversation history
	// - Prevents DetectAndInject from re-detecting the original /goal command
	// - Ensures SustainPrompt is called (not TriggerPrompt) on the next turn
	//
	// Use log+degrade pattern: notification failure should not interrupt the main flow.
	prompt := buildGoalTaskNotificationPrompt(goal)
	if err := createNotificationMessage(ctx, g.helpers.deps, ctx.SessionID, prompt); err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		g.helpers.logger.Warn(ctx, "goalWorkflow.create_notification_failed", map[string]any{
			"goal_id": goal.ID.String(),
			"error":   err.Error(),
		})
		// degrade: continue even if notification creation fails
	}

	// Re-queue a submit work item to trigger the next turn.
	// Use log+degrade pattern: re-queue failure should not interrupt the main flow, only log.
	if g.helpers.queue != nil {
		// Extract trace context for cross-process propagation.
		traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)
		payload, marshalErr := json.Marshal(turnagent.WorkPayload{
			Kind:      turnagent.WorkKindSubmit,
			SessionID: ctx.SessionID.String(),
			TraceID:   traceID,
			SpanID:    spanID,
		})
		if marshalErr != nil {
			span.SetStatus(codes.Error, marshalErr.Error())
			span.RecordError(marshalErr)
			g.helpers.logger.Warn(ctx, "goalWorkflow.marshal_failed", map[string]any{
				"goal_id": goal.ID.String(),
				"error":   marshalErr.Error(),
			})
			return nil // degrade: log but do not interrupt main flow
		}
		if _, err := g.helpers.queue.Publish(ctx, ctx.SessionID.String(), string(payload), rtcqueue.SubmitWorkPriority); err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			g.helpers.logger.Warn(ctx, "goalWorkflow.publish_failed", map[string]any{
				"goal_id":    goal.ID.String(),
				"session_id": ctx.SessionID.String(),
				"error":      err.Error(),
			})
			return nil // degrade: log but do not interrupt main flow
		}
		span.SetAttributes(attribute.String("goal.status", "extended"))
	}

	g.helpers.logger.Info(ctx, "goalWorkflow.goal_extended", map[string]any{
		"goal_id":         goal.ID.String(),
		"session_id":      ctx.SessionID.String(),
		"completed_turns": newTurns,
		"max_turns":       goal.MaxTurns,
	})
	return nil
}

// registerGoalCommand adds the /goal workflow to the registry. Called from
// agent.New after helpers is constructed, so the workflow can close over it.
func registerGoalCommand(registry *command.CommandRegistry, h *helpers) {
	if registry == nil {
		return
	}
	registry.Register(&GoalWorkflow{helpers: h, registry: registry})
}

// buildGoalTaskNotificationPrompt constructs the notification text for a goal turn continuation.
// Following the async sub-agent pattern: user-role message with <system-reminder>
// XML tags telling the LLM "this is system-level context, not user input".
//
// The notification is a simple "wake up" call. Detailed goal management instructions
// are provided by SustainPrompt (goal-management.md.tmpl), which tells the LLM to:
// - Check goal status
// - Call completeGoal if condition is satisfied
// - Call cancelGoal if condition cannot be achieved
// - Otherwise continue working toward the goal
func buildGoalTaskNotificationPrompt(goal *model.Goal) string {
	return fmt.Sprintf(`<system-reminder>
Continue working on the active goal.

Goal ID: %s
Progress: Turn %d of %d

Review the goal condition and your current progress. If the goal is complete, call the completeGoal tool to mark it as completed.
</system-reminder>`,
		goal.ID.String(),
		goal.CompletedTurns+1,
		goal.MaxTurns,
	)
}
