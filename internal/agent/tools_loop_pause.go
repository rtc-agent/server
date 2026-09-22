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
// pause_loop
// ---------------------------------------------------------------------------

type pauseLoopTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type pauseLoopResult struct {
	ID             string           `json:"id"`
	Prompt         string           `json:"prompt"`
	Status         model.LoopStatus `json:"status"`
	CompletedTurns int              `json:"completed_turns"`
	MaxTurns       int              `json:"max_turns"`
}

func (t *pauseLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        "pause_loop",
		Desc:        pauseLoopDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *pauseLoopTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.pause_loop",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	loop, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "find_active_failed")
		return "", fmt.Errorf("pause_loop: find active loop: %w", err)
	}
	if loop == nil {
		span.SetAttributes(attribute.Bool("not_found", true))
		return "Error: no active loop to pause", nil
	}

	if !loop.IsPausable() {
		span.SetAttributes(attribute.String("current_status", string(loop.Status)))
		span.SetStatus(codes.Error, "not_pausable")
		return fmt.Sprintf("Error: loop is not in a pausable state (current status: %s)", loop.Status), nil
	}

	updateFields := map[string]any{
		"status": model.LoopStatusPaused,
	}

	// Cancel the scheduled task if TaskScheduler is available.
	cancelLoopAsynqTask(ctx, t.helpers.deps, t.helpers.logger, loop, updateFields)

	if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		return "", fmt.Errorf("pause_loop: update: %w", err)
	}

	result := pauseLoopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         model.LoopStatusPaused,
		CompletedTurns: loop.CompletedTurns,
		MaxTurns:       loop.MaxTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("pause_loop: marshal result: %w", err)
	}

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "pause_loop",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("pause_loop: publish messages: %w", err)
	}

	span.SetAttributes(attribute.String("loop_id", loop.ID.String()))
	t.helpers.logger.Info(ctx, "pauseLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
	})

	return resultJSON, nil
}
