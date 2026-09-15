package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// defaultGoalMaxTurns is the fixed max_turns for create_goal.
// Per Phase 4 decision A1: not parameterizable.
const defaultGoalMaxTurns = 50

// ---------------------------------------------------------------------------
// create_goal
// ---------------------------------------------------------------------------

type createGoalTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type createGoalArgs struct {
	Condition string `json:"condition"`
}

// createGoalResult is the JSON returned to LLM and persisted in toolcall_output.
type createGoalResult struct {
	ID             string `json:"id"`
	Condition      string `json:"condition"`
	Status         string `json:"status"`
	MaxTurns       int    `json:"max_turns"`
	CompletedTurns int    `json:"completed_turns"`
}

func (t *createGoalTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "create_goal",
		Desc: createGoalDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"condition": {
				Type:     schema.String,
				Desc:     "The refined SMART completion condition (e.g., 'All 42 tests pass').",
				Required: true,
			},
		}),
	}, nil
}

func (t *createGoalTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args createGoalArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "create_goal", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Condition == "" {
		return "Error: condition is required and cannot be empty", nil
	}

	// 1. Check for existing active goal.
	existing, err := t.helpers.deps.GoalRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("create_goal: find active goal: %w", err)
	}
	if existing != nil {
		return fmt.Sprintf("Error: an active goal already exists (id=%s, condition=%q). Complete or cancel it before creating a new one.",
			existing.ID.String(), existing.Condition), nil
	}

	// 2. Check for existing active loop (mutual exclusion).
	if conflictMsg, err := checkGoalLoopMutualExclusion(ctx, t.helpers.deps, t.session.ID, "goal"); err != nil {
		return "", fmt.Errorf("create_goal: %w", err)
	} else if conflictMsg != "" {
		return conflictMsg, nil
	}

	// 3. Create the goal.
	goal := &model.Goal{
		SessionID:      t.session.ID,
		Condition:      args.Condition,
		Status:         string(model.GoalStatusActive),
		MaxTurns:       defaultGoalMaxTurns,
		CompletedTurns: 0,
		TokenUsage:     0,
	}
	if err := t.helpers.deps.GoalRepo.Create(ctx, goal); err != nil {
		return "", fmt.Errorf("create_goal: create: %w", err)
	}

	// 4. Build result + publish two messages (toolcall_input + toolcall_output).
	result := createGoalResult{
		ID:             goal.ID.String(),
		Condition:      goal.Condition,
		Status:         goal.Status,
		MaxTurns:       goal.MaxTurns,
		CompletedTurns: goal.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		return "", fmt.Errorf("create_goal: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "create_goal",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("create_goal: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "createGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"condition":  goal.Condition,
	})

	return resultJSON, nil
}

// ---------------------------------------------------------------------------
// complete_goal
// ---------------------------------------------------------------------------

type completeGoalTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type completeGoalArgs struct {
	Reason string `json:"reason"`
}

type completeGoalResult struct {
	ID             string `json:"id"`
	Condition      string `json:"condition"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	CompletedTurns int    `json:"completed_turns"`
}

func (t *completeGoalTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "complete_goal",
		Desc: completeGoalDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"reason": {
				Type:     schema.String,
				Desc:     "A brief explanation of why the goal is considered completed (e.g., 'All 42 tests pass').",
				Required: true,
			},
		}),
	}, nil
}

func (t *completeGoalTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args completeGoalArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "complete_goal", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Reason == "" {
		return "Error: reason is required and cannot be empty", nil
	}

	goal, err := t.helpers.deps.GoalRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("complete_goal: find active goal: %w", err)
	}
	if goal == nil {
		return "Error: no active goal to complete", nil
	}

	updateFields := map[string]any{
		"status":      string(model.GoalStatusCompleted),
		"last_reason": args.Reason,
	}
	if err := t.helpers.deps.GoalRepo.Update(ctx, goal.ID, updateFields); err != nil {
		return "", fmt.Errorf("complete_goal: update: %w", err)
	}

	result := completeGoalResult{
		ID:             goal.ID.String(),
		Condition:      goal.Condition,
		Status:         string(model.GoalStatusCompleted),
		Reason:         args.Reason,
		CompletedTurns: goal.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		return "", fmt.Errorf("complete_goal: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "complete_goal",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("complete_goal: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "completeGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"reason":     args.Reason,
	})

	return resultJSON, nil
}

// ---------------------------------------------------------------------------
// cancel_goal
// ---------------------------------------------------------------------------

type cancelGoalTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type cancelGoalArgs struct {
	Reason string `json:"reason"`
}

type cancelGoalResult struct {
	ID             string `json:"id"`
	Condition      string `json:"condition"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	CompletedTurns int    `json:"completed_turns"`
}

func (t *cancelGoalTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "cancel_goal",
		Desc: cancelGoalDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"reason": {
				Type:     schema.String,
				Desc:     "A brief explanation of why the goal is being cancelled.",
				Required: true,
			},
		}),
	}, nil
}

func (t *cancelGoalTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args cancelGoalArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "cancel_goal", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Reason == "" {
		return "Error: reason is required and cannot be empty", nil
	}

	goal, err := t.helpers.deps.GoalRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("cancel_goal: find active goal: %w", err)
	}
	if goal == nil {
		return "Error: no active goal to cancel", nil
	}

	updateFields := map[string]any{
		"status":      string(model.GoalStatusCancelled),
		"last_reason": args.Reason,
	}
	if err := t.helpers.deps.GoalRepo.Update(ctx, goal.ID, updateFields); err != nil {
		return "", fmt.Errorf("cancel_goal: update: %w", err)
	}

	result := cancelGoalResult{
		ID:             goal.ID.String(),
		Condition:      goal.Condition,
		Status:         string(model.GoalStatusCancelled),
		Reason:         args.Reason,
		CompletedTurns: goal.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		return "", fmt.Errorf("cancel_goal: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "cancel_goal",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("cancel_goal: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "cancelGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"reason":     args.Reason,
	})

	return resultJSON, nil
}
