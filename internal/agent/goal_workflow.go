package agent

import (
	"encoding/json"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/model"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
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

func (g *GoalWorkflow) Name() string        { return "goal" }
func (g *GoalWorkflow) Prefix() string      { return "/goal" }
func (g *GoalWorkflow) Scope() command.Scope { return command.ScopeSession }

// TriggerPrompt returns the goal creation system prompt. Called on the turn
// the user types "/goal ...".
func (g *GoalWorkflow) TriggerPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	return &command.PromptContribution{
		Role:    "system",
		Content: goalCreationPrompt,
	}, nil
}

// SustainPrompt returns the goal management user prompt when there is an
// active goal for the session. Returns nil when no goal is active or the
// goal has reached a terminal state.
func (g *GoalWorkflow) SustainPrompt(ctx command.Context, args string) (*command.PromptContribution, error) {
	if g.helpers.deps.GoalRepo == nil {
		return nil, nil
	}
	goal, err := g.helpers.deps.GoalRepo.FindActive(ctx, ctx.SessionID)
	if err != nil || goal == nil {
		return nil, nil
	}
	return &command.PromptContribution{
		Role:    "user",
		Content: buildGoalManagementPrompt(goal),
	}, nil
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
// during the confirmation phase before create_goal, or after the goal
// reaches a terminal state), making the command inert in those turns. This
// matches the legacy behavior: goal tools were always registered, and
// management prompts were only injected when an active goal existed.
//
// IMPORTANT: this hook must NOT call registry methods (Deactivate, etc.)
// because the registry holds its mutex while invoking hooks — doing so
// would deadlock.
func (g *GoalWorkflow) OnTurnComplete(ctx command.Context) error {
	if g.helpers.deps.GoalRepo == nil {
		return nil
	}

	goal, err := g.helpers.deps.GoalRepo.FindActive(ctx, ctx.SessionID)
	if err != nil {
		g.helpers.logIfEnabled(ctx, "goalWorkflow.find_active_failed", map[string]any{
			"session_id": ctx.SessionID.String(),
			"error":      err.Error(),
		})
		return nil // degrade
	}
	if goal == nil {
		// No active goal: either we're in the confirmation phase before
		// create_goal was called, or the goal was just completed/cancelled
		// by the LLM during this turn. Nothing to do — command stays
		// activated but inert.
		return nil
	}

	newTurns := goal.CompletedTurns + 1

	if newTurns > goal.MaxTurns {
		err = g.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
			return g.helpers.deps.GoalRepo.Update(ctx, goal.ID, map[string]any{
				"status":          string(model.GoalStatusExhausted),
				"completed_turns": newTurns,
			})
		})
		if err != nil {
			g.helpers.logIfEnabled(ctx, "goalWorkflow.update_exhausted_failed", map[string]any{
				"goal_id": goal.ID.String(),
				"error":   err.Error(),
			})
			return err
		}
		g.helpers.logIfEnabled(ctx, "goalWorkflow.goal_exhausted", map[string]any{
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
		g.helpers.logIfEnabled(ctx, "goalWorkflow.update_goal_failed", map[string]any{
			"goal_id": goal.ID.String(),
			"error":   err.Error(),
		})
		return err
	}

	// Re-queue a submit work item to trigger the next turn.
	if g.helpers.queue != nil {
		payload, marshalErr := json.Marshal(turnagent.WorkPayload{
			Kind:      turnagent.WorkKindSubmit,
			SessionID: ctx.SessionID.String(),
		})
		if marshalErr != nil {
			g.helpers.logIfEnabled(ctx, "goalWorkflow.marshal_failed", map[string]any{
				"goal_id": goal.ID.String(),
				"error":   marshalErr.Error(),
			})
			return marshalErr
		}
		const submitPriority int64 = 0
		if _, err := g.helpers.queue.Publish(ctx, ctx.SessionID.String(), string(payload), submitPriority); err != nil {
			g.helpers.logIfEnabled(ctx, "goalWorkflow.publish_failed", map[string]any{
				"goal_id":    goal.ID.String(),
				"session_id": ctx.SessionID.String(),
				"error":      err.Error(),
			})
			return err
		}
	}

	g.helpers.logIfEnabled(ctx, "goalWorkflow.goal_extended", map[string]any{
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
