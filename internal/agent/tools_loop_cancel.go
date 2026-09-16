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

// loopResult is the JSON-serializable response for both cancel_loop and
// complete_loop tools. Both produce structurally identical output.
type loopResult struct {
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
	return finalizeLoopStatus(ctx, t.helpers, t.session, t.turnID, "cancel_loop", "cancelLoop.completed", model.LoopStatusCancelled, args.Reason, "no active loop to cancel", argumentsInJSON)
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
	return finalizeLoopStatus(ctx, t.helpers, t.session, t.turnID, "complete_loop", "completeLoop.completed", model.LoopStatusCompleted, args.Reason, "no active loop to complete", argumentsInJSON)
}

// ---------------------------------------------------------------------------
// finalizeLoopStatus — shared implementation for cancel/complete loop tools
// ---------------------------------------------------------------------------

// finalizeLoopStatus finds the active loop, updates its status to the given
// value, cancels any scheduled asynq task, publishes tool result messages,
// and returns the JSON-serialized result.
//
// Both cancel_loop and complete_loop share identical control flow; only the
// target status, log event name, and "not found" message differ.
func finalizeLoopStatus(
	ctx context.Context,
	h *helpers,
	session *model.Session,
	turnID uuid.UUID,
	toolName string,
	logEvent string,
	status model.LoopStatus,
	reason string,
	notFoundMsg string,
	argumentsInJSON string,
) (string, error) {
	loop, err := h.deps.LoopRepo.FindActive(ctx, session.ID)
	if err != nil {
		return "", fmt.Errorf("%s: find active loop: %w", toolName, err)
	}
	if loop == nil {
		return fmt.Sprintf("Error: %s", notFoundMsg), nil
	}

	updateFields := map[string]any{
		"status":      status,
		"last_reason": reason,
	}

	// Cancel the scheduled task if TaskScheduler is available.
	cancelLoopAsynqTask(ctx, h.deps, h.logger, loop, updateFields)

	if err := h.deps.LoopRepo.Update(ctx, loop.ID, updateFields); err != nil {
		return "", fmt.Errorf("%s: update: %w", toolName, err)
	}

	result := loopResult{
		ID:             loop.ID.String(),
		Prompt:         loop.Prompt,
		Status:         status,
		Reason:         reason,
		CompletedTurns: loop.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
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
		return "", fmt.Errorf("%s: publish messages: %w", toolName, err)
	}

	h.logger.Info(ctx, logEvent, map[string]any{
		"session_id": session.ID.String(),
		"loop_id":    loop.ID.String(),
		"reason":     reason,
	})

	return resultJSON, nil
}
