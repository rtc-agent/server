package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	looppkg "github.com/rtc-agent/server/internal/loop"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"gorm.io/gorm"
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
	ID              string           `json:"id"`
	Prompt          string           `json:"prompt"`
	Status          model.LoopStatus `json:"status"`
	IntervalSeconds int              `json:"interval_seconds"`
	MaxTurns        int              `json:"max_turns"`
	CompletedTurns  int              `json:"completed_turns"`
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

	// 5. Create the loop and schedule first task in a transaction.
	// Set last_run_at to now so recovery can capture it if needed.
	now := time.Now()
	loop := &model.Loop{
		SessionID:       t.session.ID,
		Prompt:          args.Prompt,
		IntervalSeconds: intervalSeconds,
		MaxTurns:        maxTurns,
		CompletedTurns:  0,
		Status:          model.LoopStatusActive,
		ExpiresAt:       &expiresAt,
		LastRunAt:       &now,
	}

	var taskID string
	err = t.helpers.deps.DB.Transaction(func(tx *gorm.DB) error {
		// Create loop within transaction
		txCtx := repo.WithTx(ctx, tx)
		if err := t.helpers.deps.LoopRepo.Create(txCtx, loop); err != nil {
			return err
		}

		// Schedule first task within transaction (ensures atomicity)
		if t.helpers.deps.TaskScheduler != nil {
			payload, _ := json.Marshal(struct {
				LoopID    string `json:"loop_id"`
				SessionID string `json:"session_id"`
			}{
				LoopID:    loop.ID.String(),
				SessionID: loop.SessionID.String(),
			})

			delay := time.Duration(loop.IntervalSeconds) * time.Second
			id, err := t.helpers.deps.TaskScheduler.ScheduleDelayed(
				txCtx,
				looppkg.LoopTaskType,
				payload,
				delay,
			)
			if err != nil {
				return fmt.Errorf("schedule first loop task: %w", err)
			}
			taskID = id
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("create_loop: transaction failed: %w", err)
	}

	// 6. Update asynq_task_id (outside transaction since scheduling succeeded)
	if taskID != "" {
		if err := t.helpers.deps.LoopRepo.Update(ctx, loop.ID, map[string]any{
			"asynq_task_id": taskID,
		}); err != nil {
			// Non-fatal: recovery will reschedule in 5 minutes if needed
			t.helpers.logger.Warn(ctx, "createLoop.update_task_id_failed", map[string]any{
				"loop_id": loop.ID.String(),
				"task_id": taskID,
				"error":   err.Error(),
			})
		}
	}

	// 7. Build result + publish two messages (toolcall_input + toolcall_output).
	result := createLoopResult{
		ID:              loop.ID.String(),
		Prompt:          loop.Prompt,
		Status:          loop.Status,
		IntervalSeconds: loop.IntervalSeconds,
		MaxTurns:        loop.MaxTurns,
		CompletedTurns:  loop.CompletedTurns,
	}
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		return "", fmt.Errorf("create_loop: marshal result: %w", err)
	}

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
