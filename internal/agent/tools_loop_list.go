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
// list_loops
// ---------------------------------------------------------------------------

type listLoopsTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

type listLoopsArgs struct {
	Cursor *string `json:"cursor"`
	Limit  int     `json:"limit"`
}

type loopSummary struct {
	ID              string  `json:"id"`
	Prompt          string  `json:"prompt"`
	Status          string  `json:"status"`
	IntervalSeconds int     `json:"interval_seconds"`
	MaxTurns        int     `json:"max_turns"`
	CompletedTurns  int     `json:"completed_turns"`
	CreatedAt       string  `json:"created_at"`
	LastRunAt       *string `json:"last_run_at,omitempty"`
}

type listLoopsResult struct {
	Loops   []loopSummary `json:"loops"`
	HasMore bool          `json:"has_more"`
}

func (t *listLoopsTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "list_loops",
		Desc: listLoopsDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"cursor": {
				Type:     schema.String,
				Desc:     "Pagination cursor (ID of the last item from previous page).",
				Required: false,
			},
			"limit": {
				Type:     schema.Integer,
				Desc:     "Maximum number of items to return (default: 50).",
				Required: false,
			},
		}),
	}, nil
}

func (t *listLoopsTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args listLoopsArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "list_loops", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 50
	}
	// Fetch one extra to determine if there are more pages.
	loops, err := t.helpers.deps.LoopRepo.ListBySession(ctx, t.session.ID, args.Cursor, limit+1)
	if err != nil {
		return "", fmt.Errorf("list_loops: list loops: %w", err)
	}

	hasMore := len(loops) > limit
	if hasMore {
		loops = loops[:limit]
	}

	summaries := make([]loopSummary, 0, len(loops))
	for _, l := range loops {
		summary := loopSummary{
			ID:              l.ID.String(),
			Prompt:          l.Prompt,
			Status:          l.Status,
			IntervalSeconds: l.IntervalSeconds,
			MaxTurns:        l.MaxTurns,
			CompletedTurns:  l.CompletedTurns,
			CreatedAt:       l.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if l.LastRunAt != nil {
			lastRun := l.LastRunAt.Format("2006-01-02T15:04:05Z07:00")
			summary.LastRunAt = &lastRun
		}
		summaries = append(summaries, summary)
	}

	result := listLoopsResult{
		Loops:   summaries,
		HasMore: hasMore,
	}

	if err := publishLoopToolMessages(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "list_loops", argumentsInJSON, result); err != nil {
		return "", fmt.Errorf("list_loops: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "listLoops.completed", map[string]any{
		"session_id": t.session.ID.String(),
		"count":      len(summaries),
	})

	return mustMarshalJSON(result), nil
}
