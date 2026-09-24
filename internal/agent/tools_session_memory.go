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
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createSaveSessionMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &saveSessionMemoryTool{session: session, helpers: h, turnID: turnID}
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
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	var args struct {
		Category string         `json:"category"`
		Title    string         `json:"title"`
		Content  string         `json:"content"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "saveSessionMemory", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	// Validate required fields
	if args.Category == "" || args.Title == "" || args.Content == "" {
		span.SetStatus(codes.Error, "missing_required_fields")
		errMsg := "Error: category, title, and content are required"
		// 持久化验证错误到 DB，确保 checkpoint resume 时消息结构一致
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "saveSessionMemory",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "saveSessionMemory.persist_validation_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	// Validate category
	if !model.IsValidCategory(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category: %s (must be one of: decision, context, progress, issue, learnings)", args.Category)
		// 持久化验证错误到 DB
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "saveSessionMemory",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "saveSessionMemory.persist_validation_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	// Estimate token count
	tokenCount := estimateMemoryTokens(args.Content)

	// Create memory
	memory := &model.SessionMemory{
		SessionID:  t.session.ID,
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
		errMsg := fmt.Sprintf("Error: save memory: %v", err)
		// 持久化错误到 DB
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "saveSessionMemory",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "saveSessionMemory.persist_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	span.SetAttributes(
		attribute.String("memory_id", memory.ID.String()),
		attribute.Int("token_count", tokenCount),
	)
	t.helpers.logger.Info(ctx, "saveSessionMemory.success", map[string]any{
		"session_id":  t.session.ID.String(),
		"memory_id":   memory.ID.String(),
		"category":    args.Category,
		"token_count": tokenCount,
	})

	resultJSON := formatSessionMemorySaved(memory.ID.String(), args.Category, args.Title)

	// 持久化成功消息到 DB
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "saveSessionMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		t.helpers.logger.Warn(ctx, "saveSessionMemory.publish_failed", map[string]any{
			"error": err.Error(),
		})
		// 不返回错误，因为内存已保存成功
	}

	return resultJSON, nil
}

// listSessionMemoriesTool lists session memories.
type listSessionMemoriesTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createListSessionMemoriesTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &listSessionMemoriesTool{session: session, helpers: h, turnID: turnID}
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
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	var args struct {
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "listSessionMemories", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	span.SetAttributes(attribute.String("session_id", t.session.ID.String()))

	// Set default limit
	if args.Limit <= 0 {
		args.Limit = 20
	}
	span.SetAttributes(
		attribute.String("category", args.Category),
		attribute.Int("limit", args.Limit),
	)

	// Validate category if provided
	if args.Category != "" && !model.IsValidCategory(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category %q. Valid categories: %v",
			args.Category, model.ValidSessionMemoryCategories)
		// 持久化验证错误到 DB
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "listSessionMemories",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "listSessionMemories.persist_validation_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	// Query memories
	var memories []*model.SessionMemory
	var err error
	if args.Category != "" {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListByCategory(ctx, t.session.ID, args.Category, args.Limit)
	} else {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListBySession(ctx, t.session.ID, args.Limit)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		errMsg := fmt.Sprintf("Error: list memories: %v", err)
		// 持久化错误到 DB
		if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "listSessionMemories",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      errMsg,
		}); publishErr != nil {
			t.helpers.logger.Warn(ctx, "listSessionMemories.persist_error_failed", map[string]any{
				"error": publishErr.Error(),
			})
		}
		return errMsg, nil
	}

	if len(memories) == 0 {
		span.SetAttributes(attribute.Int("count", 0))
		resultJSON := formatNoSessionMemories()
		// 持久化空结果到 DB
		if err := publishToolMessages(ctx, publishToolMessagesInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          t.turnID,
			ToolName:        "listSessionMemories",
			ArgumentsInJSON: argumentsInJSON,
			ResultJSON:      resultJSON,
		}); err != nil {
			t.helpers.logger.Warn(ctx, "listSessionMemories.publish_failed", map[string]any{
				"error": err.Error(),
			})
		}
		return resultJSON, nil
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
	resultJSON := formatSessionMemoriesList(len(memories), items)

	// 持久化成功消息到 DB
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "listSessionMemories",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		t.helpers.logger.Warn(ctx, "listSessionMemories.publish_failed", map[string]any{
			"error": err.Error(),
		})
	}

	return resultJSON, nil
}
