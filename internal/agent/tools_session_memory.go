package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/stringutil"
	"github.com/rtc-agent/server/internal/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// saveSessionMemoryTool saves a session memory.
type saveSessionMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createSaveSessionMemoryTool() tool.InvokableTool {
	return &saveSessionMemoryTool{helpers: h}
}

func (t *saveSessionMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "saveSessionMemory",
		Desc: saveSessionMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"category": {
				Type:     schema.String,
				Desc:     "Category of the memory: decision, context, progress, issue, or learnings",
				Required: true,
				Enum:     []string{"decision", "context", "progress", "issue", "learnings"},
			},
			"title": {
				Type:     schema.String,
				Desc:     "Short title (5-10 words), info-dense",
				Required: true,
			},
			"content": {
				Type:     schema.String,
				Desc:     "Detailed content with specifics: file paths, function names, error messages, technical details",
				Required: true,
			},
			"metadata": {
				Type:     schema.Object,
				Desc:     "Optional metadata (related files, code snippets, etc.)",
				Required: false,
			},
		}),
	}, nil
}

func (t *saveSessionMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.saveSessionMemory",
		trace.WithAttributes(
			attribute.String("turn_id", ""),
		),
	)
	defer span.End()

	var args struct {
		Category string         `json:"category"`
		Title    string         `json:"title"`
		Content  string         `json:"content"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "saveSessionMemory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	// Extract session ID from context
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		span.SetStatus(codes.Error, "no_session_id")
		return "", fmt.Errorf("no session ID in context")
	}
	span.SetAttributes(
		attribute.String("session_id", sessionID.String()),
		attribute.String("category", args.Category),
		attribute.Int("content_length", len(args.Content)),
	)

	// Validate required fields
	if args.Category == "" || args.Title == "" || args.Content == "" {
		span.SetStatus(codes.Error, "missing_required_fields")
		return "", fmt.Errorf("category, title, and content are required")
	}

	// Validate category
	if !model.IsValidCategory(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		return "", fmt.Errorf("invalid category: %s (must be one of: decision, context, progress, issue, learnings)", args.Category)
	}

	// Estimate token count
	tokenCount := estimateMemoryTokens(args.Content)

	// Create memory
	memory := &model.SessionMemory{
		SessionID:  sessionID,
		Category:   args.Category,
		Title:      args.Title,
		Content:    args.Content,
		Metadata:   model.JSONObject(args.Metadata),
		TokenCount: &tokenCount,
	}

	// Save to database
	if err := t.helpers.deps.SessionMemoryRepo.Create(ctx, memory); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_failed")
		return "", fmt.Errorf("save memory: %w", err)
	}

	span.SetAttributes(
		attribute.String("memory_id", memory.ID.String()),
		attribute.Int("token_count", tokenCount),
	)
	t.helpers.logger.Info(ctx, "saveSessionMemory.success", map[string]any{
		"session_id":  sessionID.String(),
		"memory_id":   memory.ID.String(),
		"category":    args.Category,
		"token_count": tokenCount,
	})

	return formatSessionMemorySaved(memory.ID.String(), args.Category, args.Title), nil
}

// listSessionMemoriesTool lists session memories.
type listSessionMemoriesTool struct {
	helpers *helpers
}

func (h *helpers) createListSessionMemoriesTool() tool.InvokableTool {
	return &listSessionMemoriesTool{helpers: h}
}

func (t *listSessionMemoriesTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "listSessionMemories",
		Desc: listSessionMemoriesDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"category": {
				Type:     schema.String,
				Desc:     "Optional: filter by category (decision, context, progress, issue, learnings)",
				Required: false,
			},
			"limit": {
				Type:     schema.Integer,
				Desc:     "Optional: maximum number of memories to return (default: 20)",
				Required: false,
			},
		}),
	}, nil
}

func (t *listSessionMemoriesTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.listSessionMemories",
		trace.WithAttributes(
			attribute.String("turn_id", ""),
		),
	)
	defer span.End()

	var args struct {
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "listSessionMemories", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	// Extract session ID from context
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		span.SetStatus(codes.Error, "no_session_id")
		return "", fmt.Errorf("no session ID in context")
	}
	span.SetAttributes(attribute.String("session_id", sessionID.String()))

	// Set default limit
	if args.Limit <= 0 {
		args.Limit = 20
	}
	span.SetAttributes(
		attribute.String("category", args.Category),
		attribute.Int("limit", args.Limit),
	)

	// Query memories
	var memories []*model.SessionMemory
	var err error
	if args.Category != "" {
		if !model.IsValidCategory(args.Category) {
			span.SetStatus(codes.Error, "invalid_category")
			return fmt.Sprintf("Error: invalid category %q. Valid categories: %v",
				args.Category, model.ValidSessionMemoryCategories), nil
		}
		memories, err = t.helpers.deps.SessionMemoryRepo.ListByCategory(ctx, sessionID, args.Category, args.Limit)
	} else {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListBySession(ctx, sessionID, args.Limit)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		return "", fmt.Errorf("list memories: %w", err)
	}

	if len(memories) == 0 {
		span.SetAttributes(attribute.Int("count", 0))
		return formatNoSessionMemories(), nil
	}

	// Format output
	items := make([]sessionMemoryItem, len(memories))
	for i, mem := range memories {
		createdAt := ""
		if !mem.CreatedAt.IsZero() {
			createdAt = mem.CreatedAt.Format("2006-01-02 15:04")
		}
		items[i] = sessionMemoryItem{
			Index:     i + 1,
			Category:  mem.Category,
			Title:     mem.Title,
			Content:   stringutil.TruncateByRune(mem.Content, 200),
			CreatedAt: createdAt,
		}
	}

	span.SetAttributes(attribute.Int("count", len(memories)))
	return formatSessionMemoriesList(len(memories), items), nil
}
