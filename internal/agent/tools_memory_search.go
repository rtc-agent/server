package agent

import (
	"context"
	"sort"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/stringutil"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// searchMemoryTool searches memories.
type searchMemoryTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createSearchMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &searchMemoryTool{session: session, helpers: h, turnID: turnID}
}

func (t *searchMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "searchMemory",
		Desc: searchMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Type:     schema.String,
				Desc:     "Search query (keywords or natural language)",
				Required: true,
			},
			"category": {
				Type:     schema.String,
				Desc:     "Optional: filter by category (user, feedback, project, reference)",
				Required: false,
			},
			"limit": {
				Type:     schema.Integer,
				Desc:     "Optional: maximum number of results (default: 5)",
				Required: false,
			},
		}),
	}, nil
}

func (t *searchMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.searchMemory",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	var args struct {
		Query    string `json:"query"`
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "searchMemory", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.Query == "" {
		span.SetStatus(codes.Error, "query_required")
		errMsg := "Error: query is required"
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "searchMemory",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "searchMemory.persist_validation_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}
	span.SetAttributes(
		attribute.String("query", args.Query),
		attribute.String("category", args.Category),
	)

	// Set defaults
	if args.Limit <= 0 {
		args.Limit = 5
	}
	span.SetAttributes(attribute.Int("limit", args.Limit))

	results, err := t.searchMemories(ctx, args.Query, args.Category, args.Limit)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String("search_error", err.Error()))
		t.helpers.logger.Warn(ctx, "searchMemory.error", map[string]any{
			"error": err.Error(),
		})
		// Continue with empty results
		results = nil
	}

	var resultJSON string
	if len(results) == 0 {
		span.SetAttributes(attribute.Int("result_count", 0))
		resultJSON = formatNoSearchResults()
	} else {
		// Format output
		items := make([]searchResultItem, len(results))
		for i, result := range results {
			createdAt := ""
			if !result.CreatedAt.IsZero() {
				createdAt = result.CreatedAt.Format("2006-01-02 15:04")
			}
			items[i] = searchResultItem{
				Index:     i + 1,
				Category:  result.Category,
				Title:     result.Title,
				Content:   stringutil.TruncateByRune(result.Content, 300),
				CreatedAt: createdAt,
			}
		}

		span.SetAttributes(attribute.Int("result_count", len(results)))
		resultJSON = formatSearchResultsList(len(results), items)
	}

	// Persist results to DB.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "searchMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		t.helpers.logger.Warn(ctx, "searchMemory.publish_failed", map[string]any{
			"error": err.Error(),
		})
	}

	return resultJSON, nil
}

// searchResult is the unified search result format.
type searchResult struct {
	ID        string
	Category  string
	Title     string
	Content   string
	CreatedAt time.Time
}

// searchMemories searches memories using keyword search with importance-weighted ranking.
func (t *searchMemoryTool) searchMemories(
	ctx context.Context,
	query string,
	category string,
	limit int,
) ([]searchResult, error) {
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		return nil, err
	}

	// Keyword search.
	keywordResults, err := t.helpers.deps.MemoryRepo.Search(ctx, memory.ScopeUser, userID, query, limit*2)
	if err != nil {
		t.helpers.logger.Warn(ctx, "searchMemory.keyword_error", map[string]any{
			"error": err.Error(),
		})
		return nil, nil
	}

	// Apply importance weights and sort.
	type scoredEntry struct {
		memory *memory.Memory
		score  float64
	}
	var entries []scoredEntry
	for _, mem := range keywordResults {
		if category != "" && mem.Type != category {
			continue
		}
		entries = append(entries, scoredEntry{
			memory: mem,
			score:  model.ImportanceWeight(extractImportanceFromMetadata(mem.Metadata)),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		// Tiebreaker: newer first
		return entries[i].memory.CreatedAt.After(entries[j].memory.CreatedAt)
	})

	// Convert to unified results.
	var results []searchResult
	for i, entry := range entries {
		if i >= limit {
			break
		}
		mem := entry.memory

		results = append(results, searchResult{
			ID:        mem.ID.String(),
			Category:  mem.Type,
			Title:     mem.Title,
			Content:   mem.Content,
			CreatedAt: mem.CreatedAt,
		})
	}

	return results, nil
}
