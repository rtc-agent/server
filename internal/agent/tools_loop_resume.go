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

// ---------------------------------------------------------------------------
// resumeLoop
// ---------------------------------------------------------------------------

type resumeLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type resumeLoopResult struct {
	ID             string           `json:"id"`
	Prompt         string           `json:"prompt"`
	Status         model.LoopStatus `json:"status"`
	CompletedTurns int              `json:"completed_turns"`
	MaxTurns       int              `json:"max_turns"`
}

func (t *resumeLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        "resumeLoop",
		Desc:        resumeLoopDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *resumeLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.resumeLoop",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	// Find the paused loop (not active, not terminal).
	var pausedLoop *model.Loop
	loops, err := t.helpers.deps.LoopRepo.ListBySession(ctx, t.session.ID, nil, 10)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		return "", fmt.Errorf("resumeLoop: list loops: %w", err)
	}
	for _, l := range loops {
		if l.Status == model.LoopStatusPaused {
			pausedLoop = l
			break
		}
	}
	if pausedLoop == nil {
		span.SetAttributes(attribute.Bool("not_found", true))
		return "Error: no paused loop found to resume", nil
	}
	span.SetAttributes(attribute.String("loop_id", pausedLoop.ID.String()))

	// Check for existing active loop (only one active loop per session).
	existingActive, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "find_active_failed")
		return "", fmt.Errorf("resumeLoop: find active loop: %w", err)
	}
	if existingActive != nil {
		span.SetAttributes(attribute.Bool("conflict_active_loop", true))
		return fmt.Sprintf("Error: an active loop already exists (id=%s). Complete or cancel it before resuming another.",
			existingActive.ID.String()), nil
	}

	// Check for active goal (mutual exclusion).
	if conflictMsg, err := checkGoalLoopMutualExclusion(ctx, t.helpers.deps, t.session.ID, "loop"); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "mutual_exclusion_check_failed")
		return "", fmt.Errorf("resumeLoop: %w", err)
	} else if conflictMsg != "" {
		span.SetAttributes(attribute.Bool("conflict_active_goal", true))
		return conflictMsg, nil
	}

	if err := t.helpers.deps.LoopRepo.Update(ctx, pausedLoop.ID, map[string]any{
		"status": model.LoopStatusActive,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		return "", fmt.Errorf("resumeLoop: update: %w", err)
	}

	result := resumeLoopResult{
		ID:             pausedLoop.ID.String(),
		Prompt:         pausedLoop.Prompt,
		Status:         model.LoopStatusActive,
		CompletedTurns: pausedLoop.CompletedTurns,
		MaxTurns:       pausedLoop.MaxTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("resumeLoop: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "resumeLoop",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("resumeLoop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "resumeLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    pausedLoop.ID.String(),
	})

	return resultJSON, nil
}
