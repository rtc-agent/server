package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// ---------------------------------------------------------------------------
// cancel_loop
// ---------------------------------------------------------------------------

type cancelLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type cancelLoopArgs struct {
	Reason string `json:"reason"`
}

type cancelLoopResult struct {
	ID             string           `json:"id"`
	Prompt         string           `json:"prompt"`
	Status         model.LoopStatus `json:"status"`
	Reason         string           `json:"reason"`
	CompletedTurns int              `json:"completed_turns"`
}

func (t *cancelLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "cancel_loop",
		Desc: cancelLoopDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"reason": {
				Type:     schema.String,
				Desc:     "A brief explanation of why the loop is being cancelled.",
				Required: true,
			},
		}),
	}, nil
}

func (t *cancelLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args cancelLoopArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "cancel_loop", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Reason == "" {
		return "Error: reason is required and cannot be empty", nil
	}

	loop, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("cancel_loop: find active loop: %w", err)
	}
	if loop == nil {
		return "Error: no active loop to cancel", nil
	}

	updateFields := map[string]any{
		"status":      model.LoopStatusCancelled,
		"last_reason": args.Reason,
	}

	// Cancel the scheduled task if TaskScheduler is available.
	cancelLoopAsynqTask(ctx, t.helpers.deps, t.helpers.logger, loop, updateFields)

	if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		return "", fmt.Errorf("cancel_loop: update: %w", err)
	}

	result := cancelLoopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         model.LoopStatusCancelled,
		Reason:         args.Reason,
		CompletedTurns: loop.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		return "", fmt.Errorf("cancel_loop: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "cancel_loop",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("cancel_loop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "cancelLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"reason":     args.Reason,
	})

	return resultJSON, nil
}

// ---------------------------------------------------------------------------
// complete_loop (internal, called by LLM when objective is achieved)
// ---------------------------------------------------------------------------

type completeLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type completeLoopArgs struct {
	Reason string `json:"reason"`
}

type completeLoopResult struct {
	ID             string           `json:"id"`
	Prompt         string           `json:"prompt"`
	Status         model.LoopStatus `json:"status"`
	Reason         string           `json:"reason"`
	CompletedTurns int              `json:"completed_turns"`
}

func (t *completeLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "complete_loop",
		Desc: completeLoopDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"reason": {
				Type:     schema.String,
				Desc:     "A brief explanation of why the loop is considered completed.",
				Required: true,
			},
		}),
	}, nil
}

func (t *completeLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args completeLoopArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "complete_loop", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Reason == "" {
		return "Error: reason is required and cannot be empty", nil
	}

	loop, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("complete_loop: find active loop: %w", err)
	}
	if loop == nil {
		return "Error: no active loop to complete", nil
	}

	updateFields := map[string]any{
		"status":      model.LoopStatusCompleted,
		"last_reason": args.Reason,
	}

	// Cancel the scheduled task if TaskScheduler is available.
	cancelLoopAsynqTask(ctx, t.helpers.deps, t.helpers.logger, loop, updateFields)

	if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		return "", fmt.Errorf("complete_loop: update: %w", err)
	}

	result := completeLoopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         model.LoopStatusCompleted,
		Reason:         args.Reason,
		CompletedTurns: loop.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		return "", fmt.Errorf("complete_loop: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "complete_loop",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("complete_loop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "completeLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"reason":     args.Reason,
	})

	return resultJSON, nil
}
