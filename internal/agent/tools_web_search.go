package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/websearch"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// webSearchTool — searches the web via pkg/websearch.WebSearchManager
// ---------------------------------------------------------------------------

// webSearchTool implements Eino's InvokableTool interface for web search.
// The core search logic (load balancing, circuit breaking, failover) lives
// in pkg/websearch; this struct only handles the InvokableTool contract,
// argument parsing, tracing, and message persistence.
type webSearchTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// webSearchArgs defines the JSON arguments accepted from the LLM.
type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	TimeRange  string `json:"time_range"`
}

// webSearchResult is the JSON structure returned to the LLM and persisted
// in the toolcall_output message.
type webSearchResult struct {
	Query       string                 `json:"query"`
	ResultCount int                    `json:"result_count"`
	Results     []webSearchResultEntry `json:"results"`
}

// webSearchResultEntry is a single search result in the JSON output.
type webSearchResultEntry struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
}

// createWebSearchTool returns a webSearchTool if WebSearchManager is configured.
func (h *helpers) createWebSearchTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	if h.deps.WebSearchManager == nil {
		return nil
	}
	return &webSearchTool{session: session, helpers: h, turnID: turnID}
}

// Info returns tool metadata used by the LLM to decide whether to call.
func (t *webSearchTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "webSearch",
		Desc: webSearchDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Type:     schema.String,
				Desc:     "The search query. Should be specific and clear. Examples: 'latest Go programming news', 'Python asyncio tutorial'",
				Required: true,
			},
			"max_results": {
				Type:     schema.Integer,
				Desc:     "Maximum number of results to return (default: 10, max: 30). Use fewer results for quick answers, more for comprehensive research.",
				Required: false,
			},
			"time_range": {
				Type:     schema.String,
				Desc:     "Limit results to a specific time range. Options: 'day' (last 24h), 'week', 'month', 'year'. Leave empty for all time.",
				Required: false,
				Enum:     []string{"day", "week", "month", "year"},
			},
		}),
	}, nil
}

// InvokableRun executes the web search with JSON arguments from the LLM.
//
// Flow:
//  1. parseToolArgsWithPersist — parse JSON args (persist on parse failure)
//  2. Validate query and clamp max_results
//  3. Call WebSearchManager.Search()
//  4. publishToolMessages — persist input+output (on both success and failure)
//  5. Return JSON result to the LLM (same value as persisted for cache consistency)
func (t *webSearchTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.webSearch",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	// 1. Parse arguments.
	var args webSearchArgs
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "webSearch", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	// 2. Validate query.
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		span.SetStatus(codes.Error, "query_required")
		errMsg := "Error: query is required and cannot be empty"
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "webSearch",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "webSearch.persist_validation_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	// 3. Clamp max_results to [1, 30].
	if args.MaxResults <= 0 {
		args.MaxResults = 10
	}
	if args.MaxResults > 30 {
		args.MaxResults = 30
	}

	span.SetAttributes(
		attribute.String("query", args.Query),
		attribute.Int("max_results", args.MaxResults),
		attribute.String("time_range", args.TimeRange),
	)

	// 4. Execute search.
	req := &websearch.SearchRequest{
		Query:      args.Query,
		MaxResults: args.MaxResults,
		TimeRange:  websearch.TimeRange(args.TimeRange),
	}

	resp, err := t.helpers.deps.WebSearchManager.Search(ctx, req)
	if err != nil {
		errMsg := publishToolError(
			ctx, span, t.helpers, t.session, t.turnID,
			"webSearch",
			"webSearch.search_failed",
			"webSearch.persist_search_error_failed",
			map[string]any{"query": args.Query, "error": err.Error()},
			err,
			argumentsInJSON,
		)
		return errMsg, nil
	}

	// 5. Build result JSON.
	result := buildWebSearchResult(args.Query, resp)
	resultJSON, err := mustMarshalJSON(result)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("webSearch: marshal result: %w", err)
	}

	// 6. Persist input + output messages.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "webSearch",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("webSearch: publish messages: %w", err)
	}

	span.SetAttributes(attribute.Int("result_count", len(resp.Results)))
	t.helpers.logger.Info(ctx, "webSearch.completed", map[string]any{
		"session_id":   t.session.ID.String(),
		"query":        args.Query,
		"result_count": len(resp.Results),
		"provider":     resp.Provider,
	})

	// 7. Return JSON to the LLM (same as persisted value for cache consistency).
	return resultJSON, nil
}

// buildWebSearchResult constructs the JSON-serializable result from a
// SearchResponse.
func buildWebSearchResult(query string, resp *websearch.SearchResponse) webSearchResult {
	entries := make([]webSearchResultEntry, len(resp.Results))
	for i, r := range resp.Results {
		entries[i] = webSearchResultEntry{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Description,
			Source:      r.Source,
		}
	}
	return webSearchResult{
		Query:       query,
		ResultCount: len(resp.Results),
		Results:     entries,
	}
}

// publishToolError handles common error publishing logic for tool invocations.
// It records the error on the span, logs it, publishes an error message, and
// returns the formatted error string for the LLM.
func publishToolError(
	ctx context.Context,
	span trace.Span,
	h *helpers,
	session *model.Session,
	turnID uuid.UUID,
	toolName string,
	logEvent string,
	persistLogEvent string,
	errorContext map[string]any,
	err error,
	argumentsInJSON string,
) string {
	span.RecordError(err)
	span.SetStatus(codes.Error, "tool_failed")
	h.logger.Warn(ctx, logEvent, errorContext)

	errMsg := fmt.Sprintf("Error: %s failed: %s", toolName, err.Error())
	if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         h,
		SessionID:       session.ID,
		OwnerRefID:      session.OwnerRefID,
		TurnID:          turnID,
		ToolName:        toolName,
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      errMsg,
	}); publishErr != nil {
		h.logger.Warn(ctx, persistLogEvent, map[string]any{
			"error": publishErr.Error(),
		})
	}
	return errMsg
}
