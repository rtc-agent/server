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
// resume_loop
// ---------------------------------------------------------------------------

type resumeLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type resumeLoopResult struct {
	ID             string `json:"id"`
	Prompt         string `json:"prompt"`
	Status         string `json:"status"`
	CompletedTurns int    `json:"completed_turns"`
	MaxTurns       int    `json:"max_turns"`
}

func (t *resumeLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "resume_loop",
		Desc: "Resume a paused loop.\n\n" +
			"Call this when:\n" +
			"- The user explicitly asks to resume the loop\n" +
			"- The temporary suspension is over\n\n" +
			"The loop will continue from where it left off.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *resumeLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// Find the paused loop (not active, not terminal).
	var pausedLoop *model.Loop
	loops, err := t.helpers.deps.LoopRepo.ListBySession(ctx, t.session.ID, nil, 10)
	if err != nil {
		return "", fmt.Errorf("resume_loop: list loops: %w", err)
	}
	for _, l := range loops {
		if l.Status == string(model.LoopStatusPaused) {
			pausedLoop = l
			break
		}
	}
	if pausedLoop == nil {
		return "Error: no paused loop found to resume", nil
	}

	// Check for existing active loop (only one active loop per session).
	existingActive, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("resume_loop: find active loop: %w", err)
	}
	if existingActive != nil {
		return fmt.Sprintf("Error: an active loop already exists (id=%s). Complete or cancel it before resuming another.",
			existingActive.ID.String()), nil
	}

	// Check for active goal (mutual exclusion).
	if t.helpers.deps.GoalRepo != nil {
		activeGoal, err := t.helpers.deps.GoalRepo.FindActive(ctx, t.session.ID)
		if err != nil {
			return "", fmt.Errorf("resume_loop: find active goal: %w", err)
		}
		if activeGoal != nil {
			return fmt.Sprintf("Error: an active goal exists (id=%s). Loop and goal cannot be active simultaneously. Complete or cancel the goal first.",
				activeGoal.ID.String()), nil
		}
	}

	if err := t.helpers.deps.LoopRepo.Update(ctx, pausedLoop.ID, map[string]any{
		"status": string(model.LoopStatusActive),
	}); err != nil {
		return "", fmt.Errorf("resume_loop: update: %w", err)
	}

	result := resumeLoopResult{
		ID:             pausedLoop.ID.String(),
		Prompt:         pausedLoop.Prompt,
		Status:         string(model.LoopStatusActive),
		CompletedTurns: pausedLoop.CompletedTurns,
		MaxTurns:       pausedLoop.MaxTurns,
	}

	if err := publishLoopToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "resume_loop", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("resume_loop: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "resumeLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    pausedLoop.ID.String(),
	})

	return mustMarshalJSON(result), nil
}
