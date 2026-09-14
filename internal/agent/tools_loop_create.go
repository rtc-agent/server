package agent

import (
	"context"
	"fmt"
	"time"

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

// defaultLoopMaxTurns is the default max_turns for create_loop.
const defaultLoopMaxTurns = 10

// defaultLoopIntervalSeconds is the default interval for create_loop.
const defaultLoopIntervalSeconds = 60

// loopExpiryDays is the number of days before a loop auto-expires.
const loopExpiryDays = 7

// ---------------------------------------------------------------------------
// create_loop
// ---------------------------------------------------------------------------

type createLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type createLoopArgs struct {
	Prompt          string `json:"prompt"`
	IntervalSeconds int    `json:"interval_seconds"`
	MaxTurns        int    `json:"max_turns"`
}

// createLoopResult is the JSON returned to LLM and persisted in toolcall_output.
type createLoopResult struct {
	ID              string `json:"id"`
	Prompt          string `json:"prompt"`
	Status          string `json:"status"`
	IntervalSeconds int    `json:"interval_seconds"`
	MaxTurns        int    `json:"max_turns"`
	CompletedTurns  int    `json:"completed_turns"`
}

func (t *createLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "create_loop",
		Desc: createLoopDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"prompt": {
				Type:     schema.String,
				Desc:     "The task to execute each turn (e.g., 'Check deployment health and report status').",
				Required: true,
			},
			"interval_seconds": {
				Type:     schema.Integer,
				Desc:     "Seconds between executions (default: 60).",
				Required: false,
			},
			"max_turns": {
				Type:     schema.Integer,
				Desc:     "Maximum number of executions (default: 10).",
				Required: false,
			},
		}),
	}, nil
}

func (t *createLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args createLoopArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "create_loop", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Prompt == "" {
		return "Error: prompt is required and cannot be empty", nil
	}

	// 1. Check for existing active loop.
	existing, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("create_loop: find active loop: %w", err)
	}
	if existing != nil {
		return fmt.Sprintf("Error: an active loop already exists (id=%s, prompt=%q). Complete or cancel it before creating a new one.",
			existing.ID.String(), existing.Prompt), nil
	}

	// 2. Check for existing active goal (mutual exclusion).
	if t.helpers.deps.GoalRepo != nil {
		activeGoal, err := t.helpers.deps.GoalRepo.FindActive(ctx, t.session.ID)
		if err != nil {
			return "", fmt.Errorf("create_loop: find active goal: %w", err)
		}
		if activeGoal != nil {
			return fmt.Sprintf("Error: an active goal exists (id=%s). Loop and goal cannot be active simultaneously. Complete or cancel the goal first.",
				activeGoal.ID.String()), nil
		}
	}

	// 3. Apply defaults.
	intervalSeconds := args.IntervalSeconds
	if intervalSeconds <= 0 {
		intervalSeconds = defaultLoopIntervalSeconds
	}
	maxTurns := args.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultLoopMaxTurns
	}

	// 4. Calculate expiry time.
	expiresAt := time.Now().Add(loopExpiryDays * 24 * time.Hour)

	// 5. Create the loop.
	loop := &model.Loop{
		SessionID:       t.session.ID,
		Prompt:          args.Prompt,
		IntervalSeconds: intervalSeconds,
		MaxTurns:        maxTurns,
		CompletedTurns:  0,
		Status:          string(model.LoopStatusActive),
		ExpiresAt:       &expiresAt,
	}
	if err := t.helpers.deps.LoopRepo.Create(ctx, loop); err != nil {
		return "", fmt.Errorf("create_loop: create: %w", err)
	}

	// 6. Build result + publish two messages (toolcall_input + toolcall_output).
	result := createLoopResult{
		ID:              loop.ID.String(),
		Prompt:          loop.Prompt,
		Status:          loop.Status,
		IntervalSeconds: loop.IntervalSeconds,
		MaxTurns:        loop.MaxTurns,
		CompletedTurns:  loop.CompletedTurns,
	}

	if err := publishLoopToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "create_loop", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("create_loop: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "createLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"prompt":     loop.Prompt,
	})

	return mustMarshalJSON(result), nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// publishLoopToolMessages creates the toolcall_input + toolcall_output messages
// for a loop tool invocation and publishes them (with EntityMessage.created events)
// in a single transaction. Mirrors the pattern in publishGoalToolMessages.
//
// ownerRefID is the session owner's reference ID, used to build the Centrifuge
// user topic for the publish events.
func publishLoopToolMessages(
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
