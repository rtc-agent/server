package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
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
		Desc: "Create a persistent goal for the current session. The agent will keep working across turns until the condition is met or the goal is cancelled.\n\n" +
			"Call this ONLY after:\n" +
			"1. Investigating the current state (run tests, inspect files, etc.)\n" +
			"2. Refining the user's intent into a SMART condition (Specific, Measurable, Achievable, Relevant, Time-bound)\n" +
			"3. Proposing the condition to the user and receiving explicit confirmation\n\n" +
			"IMPORTANT: You MUST NOT call this tool if there is already an active goal. " +
			"You MUST NOT decide the condition for the user — user confirmation is required.",
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

	// 2. Create the goal.
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

	// 3. Build result + publish two messages (toolcall_input + toolcall_output).
	result := createGoalResult{
		ID:             goal.ID.String(),
		Condition:      goal.Condition,
		Status:         goal.Status,
		MaxTurns:       goal.MaxTurns,
		CompletedTurns: goal.CompletedTurns,
	}

	if err := publishGoalToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "create_goal", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("create_goal: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "createGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"condition":  goal.Condition,
	})

	return mustMarshalJSON(result), nil
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
		Desc: "Mark the current active goal as completed.\n\n" +
			"Call this when you have VERIFIED that the goal condition is fully satisfied " +
			"(e.g., run tests, check outputs, lint code). " +
			"Do NOT skip verification steps before calling this tool.\n\n" +
			"IMPORTANT: This is a mandatory step. If the goal condition is met, you MUST call this tool. Do not skip it.",
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

	if err := publishGoalToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "complete_goal", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("complete_goal: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "completeGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"reason":     args.Reason,
	})

	return mustMarshalJSON(result), nil
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
		Desc: "Cancel the current active goal.\n\n" +
			"Call this when:\n" +
			"- The user explicitly asks to stop (e.g., '停下', '算了', 'cancel')\n" +
			"- You determine the goal is impossible to achieve\n\n" +
			"IMPORTANT: After calling this tool, end your turn immediately.",
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

	if err := publishGoalToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "cancel_goal", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("cancel_goal: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "cancelGoal.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"goal_id":    goal.ID.String(),
		"reason":     args.Reason,
	})

	return mustMarshalJSON(result), nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// publishGoalToolMessages creates the toolcall_input + toolcall_output messages
// for a goal tool invocation and publishes them (with EntityMessage.created events)
// in a single transaction. Mirrors the pattern in stopSubAgentTool.
//
// ownerRefID is the session owner's reference ID, used to build the Centrifuge
// user topic for the publish events.
func publishGoalToolMessages(
	ctx context.Context,
	h *helpers,
	sessionID uuid.UUID,
	ownerRefID string,
	turnID uuid.UUID,
	toolName string,
	argumentsInJSON string,
	result any,
) error {
	resultJSON := mustMarshalJSON(result)
	completedStatus := "completed"

	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return fmt.Errorf("%s: tool_call_id not set in context", toolName)
	}
	if turnID == uuid.Nil {
		return fmt.Errorf("%s: turn UUID is nil", toolName)
	}

	inputToolCall := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: toolName,
		Input:    argumentsInJSON,
	}
	inputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: inputToolCall,
	}

	outputToolCall := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: toolName,
		Input:    argumentsInJSON,
		Output:   &resultJSON,
		Status:   &completedStatus,
	}
	outputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: outputToolCall,
	}

	_, err := h.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		inputMsg, createErr := primitives.CreateMessage(
			txCtx, h.deps,
			sessionID, &turnID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			inputContent,
			protocol.MessageStreamingCompleted,
			"",  // system-generated
			nil, // no parent
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_input: %w", createErr)
		}
		inputMsgID := inputMsg.ID

		outputMsg, createErr := primitives.CreateMessage(
			txCtx, h.deps,
			sessionID, &turnID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			outputContent,
			protocol.MessageStreamingCompleted,
			"",          // system-generated
			&inputMsgID, // parent = toolcall_input
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_output: %w", createErr)
		}

		ch := channel.UserTopic(ownerRefID)
		updateItems := []updates.UpdatePublishItem{
			{
				Channel: ch,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(inputMsgID.String()),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(outputMsg.ID.String()),
					},
				},
			},
		}
		return updateItems, nil
	})
	return err
}

// mustMarshalJSON marshals v to JSON, panicking on error (programmer error).
func mustMarshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("marshal json: %w", err))
	}
	return string(b)
}
