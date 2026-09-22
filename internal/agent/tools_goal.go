package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// defaultGoalMaxTurns is the fixed max_turns for createGoal.
// Per Phase 4 decision A1: not parameterizable.
const defaultGoalMaxTurns = 50

// ---------------------------------------------------------------------------
// createGoal
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
	ID             string           `json:"id"`
	Condition      string           `json:"condition"`
	Status         model.GoalStatus `json:"status"`
	MaxTurns       int              `json:"max_turns"`
	CompletedTurns int              `json:"completed_turns"`
}

func (t *createGoalTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "createGoal",
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
	ctx, span := t.helpers.tracer.Start(ctx, "tool.createGoal",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	var args createGoalArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "createGoal", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Condition == "" {
		return "Error: condition is required and cannot be empty", nil
	}
	span.SetAttributes(attribute.Int("condition_length", len(args.Condition)))

	// 1. Check for existing active goal.
	existing, err := t.helpers.deps.GoalRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "find_active_failed")
		return "", fmt.Errorf("createGoal: find active goal: %w", err)
	}
	if existing != nil {
		span.SetAttributes(attribute.Bool("conflict", true))
		return fmt.Sprintf("Error: an active goal already exists (id=%s, condition=%q). Complete or cancel it before creating a new one.",
			existing.ID.String(), existing.Condition), nil
	}

	// 2. Check for existing active loop (mutual exclusion).
	if conflictMsg, err := checkGoalLoopMutualExclusion(ctx, t.helpers.deps, t.session.ID, "goal"); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "mutual_exclusion_check_failed")
		return "", fmt.Errorf("createGoal: %w", err)
	} else if conflictMsg != "" {
		span.SetAttributes(attribute.Bool("conflict", true))
		return conflictMsg, nil
	}

	// 3. Create the goal.
	goal := &model.Goal{
		SessionID:      t.session.ID,
		Condition:      args.Condition,
		Status:         model.GoalStatusActive,
		MaxTurns:       defaultGoalMaxTurns,
		CompletedTurns: 0,
		TokenUsage:     0,
	}
	if err := t.helpers.deps.GoalRepo.Create(ctx, goal); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_failed")
		return "", fmt.Errorf("createGoal: create: %w", err)
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("createGoal: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "createGoal",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("createGoal: publish messages: %w", err)
	}

	span.SetAttributes(attribute.String("goal_id", goal.ID.String()))
	t.helpers.logger.Info(ctx, "createGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"condition":  goal.Condition,
	})

	return resultJSON, nil
}

// ---------------------------------------------------------------------------
// completeGoal
// ---------------------------------------------------------------------------

type completeGoalTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type completeGoalArgs struct {
	Reason string `json:"reason"`
}

// goalResult is the JSON returned to LLM for both completeGoal and
// cancelGoal tools. Both produce structurally identical output.
type goalResult struct {
	ID             string           `json:"id"`
	Condition      string           `json:"condition"`
	Status         model.GoalStatus `json:"status"`
	Reason         string           `json:"reason"`
	CompletedTurns int              `json:"completed_turns"`
}

func (t *completeGoalTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "completeGoal",
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
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "completeGoal", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Reason == "" {
		return "Error: reason is required and cannot be empty", nil
	}
	return finalizeGoalStatus(ctx, t.helpers, t.session, t.turnID, "completeGoal", "completeGoal.completed", model.GoalStatusCompleted, args.Reason, "no active goal to complete", argumentsInJSON)
}

// ---------------------------------------------------------------------------
// cancelGoal
// ---------------------------------------------------------------------------

type cancelGoalTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type cancelGoalArgs struct {
	Reason string `json:"reason"`
}

func (t *cancelGoalTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "cancelGoal",
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
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "cancelGoal", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Reason == "" {
		return "Error: reason is required and cannot be empty", nil
	}
	return finalizeGoalStatus(ctx, t.helpers, t.session, t.turnID, "cancelGoal", "cancelGoal.completed", model.GoalStatusCancelled, args.Reason, "no active goal to cancel", argumentsInJSON)
}

// ---------------------------------------------------------------------------
// finalizeGoalStatus — shared implementation for complete/cancel goal tools
// ---------------------------------------------------------------------------

// finalizeGoalStatus finds the active goal, updates its status, publishes tool
// result messages, and returns the JSON-serialized result.
//
// Both completeGoal and cancelGoal share identical control flow; only the
// target status, log event name, and "not found" message differ.
func finalizeGoalStatus(
	ctx context.Context,
	h *helpers,
	session *model.Session,
	turnID uuid.UUID,
	toolName string,
	logEvent string,
	status model.GoalStatus,
	reason string,
	notFoundMsg string,
	argumentsInJSON string,
) (string, error) {
	ctx, span := h.tracer.Start(ctx, "tool."+toolName,
		trace.WithAttributes(
			attribute.String("session_id", session.ID.String()),
			attribute.String("turn_id", turnID.String()),
			attribute.String("target_status", string(status)),
			attribute.Int("reason_length", len(reason)),
		),
	)
	defer span.End()

	goal, err := h.deps.GoalRepo.FindActive(ctx, session.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "find_active_failed")
		return "", fmt.Errorf("%s: find active goal: %w", toolName, err)
	}
	if goal == nil {
		span.SetAttributes(attribute.Bool("not_found", true))
		return fmt.Sprintf("Error: %s", notFoundMsg), nil
	}

	updateFields := map[string]any{
		"status":      status,
		"last_reason": reason,
	}
	if err := h.deps.GoalRepo.Update(ctx, goal.ID, updateFields); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		return "", fmt.Errorf("%s: update: %w", toolName, err)
	}

	result := goalResult{
		ID:             goal.ID.String(),
		Condition:      goal.Condition,
		Status:         status,
		Reason:         reason,
		CompletedTurns: goal.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("%s: marshal result: %w", toolName, err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         h,
		SessionID:       session.ID,
		OwnerRefID:      session.OwnerRefID,
		TurnID:          turnID,
		ToolName:        toolName,
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("%s: publish messages: %w", toolName, err)
	}

	span.SetAttributes(attribute.String("goal_id", goal.ID.String()))
	h.logger.Info(ctx, logEvent, map[string]any{
		"session_id": session.ID.String(),
		"goal_id":    goal.ID.String(),
		"reason":     reason,
	})

	return resultJSON, nil
}
