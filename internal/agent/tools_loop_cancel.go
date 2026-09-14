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
	ID             string `json:"id"`
	Prompt         string `json:"prompt"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	CompletedTurns int    `json:"completed_turns"`
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
		"status":      string(model.LoopStatusCancelled),
		"last_reason": args.Reason,
	}

	// Cancel the scheduled task if TaskScheduler is available.
	if t.helpers.deps.TaskScheduler != nil && loop.AsynqTaskID != "" {
		if cancelErr := t.helpers.deps.TaskScheduler.Cancel(ctx, loop.AsynqTaskID); cancelErr != nil {
			t.helpers.logger.Info(ctx, "cancelLoop.cancel_task_failed", map[string]any{
				"loop_id": loop.ID.String(),
				"task_id": loop.AsynqTaskID,
				"error":   cancelErr.Error(),
			})
			// Continue with cancellation even if task cancellation fails.
		}
		updateFields["asynq_task_id"] = ""
	}

	if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		return "", fmt.Errorf("cancel_loop: update: %w", err)
	}

	result := cancelLoopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         string(model.LoopStatusCancelled),
		Reason:         args.Reason,
		CompletedTurns: loop.CompletedTurns,
	}

	if err := publishLoopToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "cancel_loop", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("cancel_loop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "cancelLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"reason":     args.Reason,
	})

	return mustMarshalJSON(result), nil
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
	ID             string `json:"id"`
	Prompt         string `json:"prompt"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	CompletedTurns int    `json:"completed_turns"`
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
		"status":      string(model.LoopStatusCompleted),
		"last_reason": args.Reason,
	}

	// Cancel the scheduled task if TaskScheduler is available.
	if t.helpers.deps.TaskScheduler != nil && loop.AsynqTaskID != "" {
		if cancelErr := t.helpers.deps.TaskScheduler.Cancel(ctx, loop.AsynqTaskID); cancelErr != nil {
			t.helpers.logger.Info(ctx, "completeLoop.cancel_task_failed", map[string]any{
				"loop_id": loop.ID.String(),
				"task_id": loop.AsynqTaskID,
				"error":   cancelErr.Error(),
			})
			// Continue with completion even if task cancellation fails.
		}
		updateFields["asynq_task_id"] = ""
	}

	if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		return "", fmt.Errorf("complete_loop: update: %w", err)
	}

	result := completeLoopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         string(model.LoopStatusCompleted),
		Reason:         args.Reason,
		CompletedTurns: loop.CompletedTurns,
	}

	if err := publishLoopToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "complete_loop", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("complete_loop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "completeLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"reason":     args.Reason,
	})

	return mustMarshalJSON(result), nil
}
