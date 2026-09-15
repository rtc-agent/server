package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
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
	if conflictMsg, err := checkGoalLoopMutualExclusion(ctx, t.helpers.deps, t.session.ID, "loop"); err != nil {
		return "", fmt.Errorf("create_loop: %w", err)
	} else if conflictMsg != "" {
		return conflictMsg, nil
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
	resultJSON := mustMarshalJSON(result)

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "create_loop",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("create_loop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "createLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"prompt":     loop.Prompt,
	})

	return resultJSON, nil
}
