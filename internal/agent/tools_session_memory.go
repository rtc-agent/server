package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
		// Persist validation error to DB to ensure message structure consistency on checkpoint resume.
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
	if !memory.IsValidMemoryType(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category: %s (must be one of: decision, context, progress, issue, learnings)", args.Category)
		// Persist validation error to DB.
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
	mem := &memory.Memory{
		Scope:      memory.ScopeSession,
		ScopeID:    t.session.ID,
		Type:       args.Category,
		Title:      args.Title,
		Content:    args.Content,
		Timestamp:  time.Now(),
		TokenCount: tokenCount,
	}
	if len(args.Metadata) > 0 {
		jsonBytes, err := json.Marshal(args.Metadata)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "marshal_metadata_failed")
			errMsg := fmt.Sprintf("Error: marshal metadata: %v", err)
			if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
				Helpers:         t.helpers,
				SessionID:       t.session.ID,
				OwnerRefID:      t.session.OwnerRefID,
				TurnID:          t.turnID,
				ToolName:        "saveSessionMemory",
				ArgumentsInJSON: argumentsInJSON,
				ResultJSON:      errMsg,
			}); publishErr != nil {
				t.helpers.logger.Warn(ctx, "saveSessionMemory.persist_metadata_error_failed", map[string]any{
					"error": publishErr.Error(),
				})
			}
			return errMsg, nil
		}
		mem.Metadata = memory.JSONBString(jsonBytes)
	}

	// Save to database
	if err := t.helpers.deps.MemoryRepo.Create(ctx, mem); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_failed")
		errMsg := fmt.Sprintf("Error: save memory: %v", err)
		// Persist error to DB.
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
		attribute.String("memory_id", mem.ID.String()),
		attribute.Int("token_count", tokenCount),
	)
	t.helpers.logger.Info(ctx, "saveSessionMemory.success", map[string]any{
		"session_id":  t.session.ID.String(),
		"memory_id":   mem.ID.String(),
		"category":    args.Category,
		"token_count": tokenCount,
	})

	resultJSON := formatSessionMemorySaved(mem.ID.String(), args.Category, args.Title)

	// Persist success message to DB.
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
		// Do not return error since the memory was already saved.
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
	if args.Category != "" && !memory.IsValidMemoryType(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category %q. Valid categories: %v",
			args.Category, memory.ValidMemoryTypes)
		// Persist validation error to DB.
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
	var memories []*memory.Memory
	var err error
	if args.Category != "" {
		memories, err = t.helpers.deps.MemoryRepo.ListByScope(ctx, memory.ScopeSession, t.session.ID, memory.ListOptions{Type: args.Category, Limit: args.Limit})
	} else {
		memories, err = t.helpers.deps.MemoryRepo.ListByScope(ctx, memory.ScopeSession, t.session.ID, memory.ListOptions{Limit: args.Limit})
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		errMsg := fmt.Sprintf("Error: list memories: %v", err)
		// Persist error to DB.
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
		// Persist empty result to DB.
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
			Category:  mem.Type,
			Title:     mem.Title,
			Content:   stringutil.TruncateByRune(mem.Content, 200),
			CreatedAt: createdAt,
		}
	}

	span.SetAttributes(attribute.Int("count", len(memories)))
	resultJSON := formatSessionMemoriesList(len(memories), items)

	// Persist success message to DB.
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
