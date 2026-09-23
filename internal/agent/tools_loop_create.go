package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"gorm.io/gorm"
)

// defaultLoopMaxTurns is the default max_turns for createLoop.
const defaultLoopMaxTurns = 10

// defaultLoopIntervalSeconds is the default interval for createLoop.
const defaultLoopIntervalSeconds = 60

// loopExpiryDays is the number of days before a loop auto-expires.
const loopExpiryDays = 7

// ---------------------------------------------------------------------------
// createLoop
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

func (t *createLoopTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "createLoop",
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "createLoop", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}
	if args.Prompt == "" {
		return "Error: prompt is required and cannot be empty", nil
	}

	// 1. Check for existing active loop.
	existing, err := t.helpers.deps.LoopRepo.FindActive(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("createLoop: find active loop: %w", err)
	}
	if existing != nil {
		return fmt.Sprintf("Error: an active loop already exists (id=%s, prompt=%q). Complete or cancel it before creating a new one.",
			existing.ID.String(), existing.Prompt), nil
	}

	// 2. Check for existing active goal (mutual exclusion).
	if conflictMsg, err := checkGoalLoopMutualExclusion(ctx, t.helpers.deps, t.session.ID, "loop"); err != nil {
		return "", fmt.Errorf("createLoop: %w", err)
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

	// 5. Create the loop and schedule first task in a transaction.
	// Set last_run_at to now so recovery can capture it if needed.
	now := time.Now()
	loop := &model.Loop{
		SessionID:       t.session.ID,
		Prompt:          args.Prompt,
		IntervalSeconds: intervalSeconds,
		MaxTurns:        maxTurns,
		CompletedTurns:  0, // 0 turns completed; will be incremented to 1 after immediate execution
		Status:          model.LoopStatusActive,
		ExpiresAt:       &expiresAt,
		LastRunAt:       &now,
	}

	err = t.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
		// Create loop within transaction
		txCtx := repo.WithTx(ctx, tx)
		if err := t.helpers.deps.LoopRepo.Create(txCtx, loop); err != nil {
			return err
		}

		// No need to schedule the first asynq task here.
		// The LLM will immediately execute the first turn after createLoop returns
		// (as instructed by the loop-creation.md prompt).
		// When that turn completes, OnTurnComplete will increment completed_turns
		// and schedule the next asynq task automatically.
		// This avoids redundant scheduling and keeps the flow simple.

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("createLoop: transaction failed: %w", err)
	}

	// Note: asynq_task_id will be set by OnTurnComplete after the first turn completes

	// 7. Build formatted message for LLM context (also persisted for cache consistency).
	// The event stream receives this formatted text, not raw JSON.
	formattedResult := formatLoopCreated(loop.ID.String(), loop.Prompt, loop.IntervalSeconds, loop.MaxTurns)

	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "createLoop",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      formattedResult,
	}); err != nil {
		return "", fmt.Errorf("createLoop: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "createLoop.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"loop_id":    loop.ID.String(),
		"prompt":     loop.Prompt,
	})

	return formattedResult, nil
}
