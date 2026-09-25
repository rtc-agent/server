package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// SearchArguments defines the JSON arguments from LLM
type SearchArguments struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	TimeRange  string `json:"time_range"`
}

// webSearchTool implements Eino's InvokableTool interface
type webSearchTool struct {
	manager *WebSearchManager
	info    *schema.ToolInfo
	logger  *zap.Logger
}

// NewWebSearchTool creates an Eino-compatible web search tool
// description is the tool description loaded from embedded markdown file (go:embed)
func NewWebSearchTool(manager *WebSearchManager, logger *zap.Logger, description string) (tool.InvokableTool, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	info, err := buildToolInfo(description)
	if err != nil {
		return nil, fmt.Errorf("build tool info: %w", err)
	}

	return &webSearchTool{
		manager: manager,
		info:    info,
		logger:  logger,
	}, nil
}

// Info returns tool metadata (used by LLM to decide whether to call)
func (t *webSearchTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

// InvokableRun executes the tool with JSON arguments from LLM
func (t *webSearchTool) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	opts ...tool.Option,
) (string, error) {
	start := time.Now()

	// Parse arguments
	var args SearchArguments
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		t.logger.Error("invalid arguments", zap.Error(err), zap.String("args", argumentsInJSON))
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Validate query
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return "", fmt.Errorf("query must not be empty")
	}

	// Validate MaxResults
	if args.MaxResults <= 0 {
		args.MaxResults = 10
	}
	if args.MaxResults > 30 {
		args.MaxResults = 30
	}

	t.logger.Info("web search request",
		zap.String("query", args.Query),
		zap.Int("max_results", args.MaxResults),
	)

	// Build search request
	req := &SearchRequest{
		Query:      args.Query,
		MaxResults: args.MaxResults,
		TimeRange:  TimeRange(args.TimeRange),
	}

	// Execute search
	resp, err := t.manager.Search(ctx, req)
	duration := time.Since(start)

	if err != nil {
		t.logger.Error("web search failed",
			zap.Error(err),
			zap.String("query", args.Query),
			zap.Duration("duration", duration),
		)
		return "", fmt.Errorf("search failed: %w", err)
	}

	t.logger.Info("web search success",
		zap.String("query", args.Query),
		zap.Int("results", len(resp.Results)),
		zap.Duration("duration", duration),
	)

	return formatSearchResponse(resp), nil
}

// buildToolInfo constructs tool metadata for LLM
// description is loaded from embedded markdown file via go:embed in agent package
func buildToolInfo(description string) (*schema.ToolInfo, error) {
	if description == "" {
		description = "Search the web for current information."
	}

	params := schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
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
	})

	return &schema.ToolInfo{
		Name:        "web_search",
		Desc:        description,
		ParamsOneOf: params,
	}, nil
}

// formatSearchResponse formats search results for LLM consumption
func formatSearchResponse(resp *SearchResponse) string {
	if len(resp.Results) == 0 {
		return "No search results found."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results:\n\n", len(resp.Results)))

	for i, result := range resp.Results {
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, result.Title))
		sb.WriteString(fmt.Sprintf("   URL: %s\n", result.URL))
		if result.Description != "" {
			snippet := truncate(result.Description, 500)
			sb.WriteString(fmt.Sprintf("   %s\n", snippet))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// truncate shortens strings to maxLen characters (UTF-8 safe)
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-3]) + "..."
}
