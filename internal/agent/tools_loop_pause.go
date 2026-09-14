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
// pause_loop
// ---------------------------------------------------------------------------

type pauseLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type pauseLoopResult struct {
	ID             string `json:"id"`
	Prompt         string `json:"prompt"`
	Status         string `json:"status"`
	CompletedTurns int    `json:"completed_turns"`
	MaxTurns       int    `json:"max_turns"`
}

func (t *pauseLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "pause_loop",
		Desc: pauseLoopDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *pauseLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	loop, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("pause_loop: find active loop: %w", err)
	}
	if loop == nil {
		return "Error: no active loop to pause", nil
	}

	if !loop.IsPausable() {
		return fmt.Sprintf("Error: loop is not in a pausable state (current status: %s)", loop.Status), nil
	}

	updateFields := map[string]any{
		"status": string(model.LoopStatusPaused),
	}

	// Cancel the scheduled task if TaskScheduler is available.
	if t.helpers.deps.TaskScheduler != nil && loop.AsynqTaskID != "" {
		if cancelErr := t.helpers.deps.TaskScheduler.Cancel(ctx, loop.AsynqTaskID); cancelErr != nil {
			t.helpers.logger.Info(ctx, "pauseLoop.cancel_task_failed", map[string]any{
				"loop_id": loop.ID.String(),
				"task_id": loop.AsynqTaskID,
				"error":   cancelErr.Error(),
			})
			// Continue with pause even if task cancellation fails.
		}
		updateFields["asynq_task_id"] = ""
	}

	if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		return "", fmt.Errorf("pause_loop: update: %w", err)
	}

	result := pauseLoopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         string(model.LoopStatusPaused),
		CompletedTurns: loop.CompletedTurns,
		MaxTurns:       loop.MaxTurns,
	}

	if err := publishLoopToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "pause_loop", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("pause_loop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "pauseLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
	})

	return mustMarshalJSON(result), nil
}
