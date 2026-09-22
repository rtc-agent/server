package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/stringutil"
	"github.com/rtc-agent/server/internal/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// getUserIDFromContext gets the user ID from context.
// Obtained by looking up the current session's owner.
func (h *helpers) getUserIDFromContext(ctx context.Context) (uuid.UUID, error) {
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("no session ID in context")
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get session: %w", err)
	}

	// OwnerRefID is the string representation of the user ID.
	userID, err := uuid.Parse(session.OwnerRefID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse owner_ref_id %q as UUID: %w", session.OwnerRefID, err)
	}

	return userID, nil
}

// =============================================================================
// save_user_memory
// =============================================================================

type saveUserMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createSaveUserMemoryTool() tool.InvokableTool {
	return &saveUserMemoryTool{helpers: h}
}

func (t *saveUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "save_user_memory",
		Desc: saveUserMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"category": {
				Type:     schema.String,
				Desc:     "Category: user, feedback, project, or reference",
				Required: true,
				Enum:     model.ValidUserMemoryCategories,
			},
			"importance": {
				Type:     schema.String,
				Desc:     "Importance level: low, medium, high, critical (default: medium)",
				Required: false,
				Enum:     model.ValidImportances,
			},
			"title": {
				Type:     schema.String,
				Desc:     "Short descriptive title (5-15 words, kebab-case style)",
				Required: true,
			},
			"content": {
				Type:     schema.String,
				Desc:     "Detailed content. For feedback/project: must include '**Why:** ...' and '**How to apply:** ...' sections",
				Required: true,
			},
			"description": {
				Type:     schema.String,
				Desc:     "One-line summary for retrieval (max 200 chars)",
				Required: false,
			},
			"tags": {
				Type:     schema.Array,
				Desc:     "Tags for keyword-based retrieval",
				Required: false,
			},
			"metadata": {
				Type:     schema.Object,
				Desc:     "Optional metadata (related files, URLs, etc.)",
				Required: false,
			},
		}),
	}, nil
}

func (t *saveUserMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.save_user_memory",
		trace.WithAttributes(
			attribute.String("turn_id", ""),
		),
	)
	defer span.End()

	var args struct {
		Category    string         `json:"category"`
		Importance  string         `json:"importance"`
		Title       string         `json:"title"`
		Content     string         `json:"content"`
		Description *string        `json:"description,omitempty"`
		Tags        []string       `json:"tags,omitempty"`
		Metadata    map[string]any `json:"metadata,omitempty"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "save_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}
	if args.Category == "" || args.Title == "" || args.Content == "" {
		span.SetStatus(codes.Error, "missing_required_fields")
		return "", fmt.Errorf("category, title, and content are required")
	}
	span.SetAttributes(
		attribute.String("category", args.Category),
		attribute.String("importance", args.Importance),
		attribute.Int("content_length", len(args.Content)),
	)

	// Validate category
	if !model.IsValidUserMemoryCategory(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		return "", fmt.Errorf("invalid category: %s (must be one of: %s)",
			args.Category, strings.Join(model.ValidUserMemoryCategories, ", "))
	}

	// Validate importance
	if args.Importance == "" {
		args.Importance = model.ImportanceMedium
	}
	if !model.IsValidImportance(args.Importance) {
		span.SetStatus(codes.Error, "invalid_importance")
		return "", fmt.Errorf("invalid importance: %s (must be one of: %s)",
			args.Importance, strings.Join(model.ValidImportances, ", "))
	}

	// Tool-layer validation: feedback and project must include Why/How structure
	if args.Category == model.UserMemoryCategoryFeedback || args.Category == model.UserMemoryCategoryProject {
		if err := validateStructuredContent(args.Content); err != nil {
			span.SetStatus(codes.Error, "content_validation_failed")
			return "", fmt.Errorf("content validation failed for %s category: %w", args.Category, err)
		}
	}

	// Get user ID from context
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_user_id_failed")
		return "", fmt.Errorf("get user ID: %w", err)
	}
	span.SetAttributes(attribute.String("user_id", userID.String()))

	// Get session ID for source tracking
	sessionID := getSessionIDFromContext(ctx)

	// Create memory
	memory := &model.UserMemory{
		UserID:          userID,
		Category:        args.Category,
		Importance:      args.Importance,
		Title:           args.Title,
		Content:         args.Content,
		Description:     args.Description,
		Tags:            model.StringArray(args.Tags),
		Metadata:        model.JSONObject(args.Metadata),
		SourceSessionID: &sessionID,
	}

	if err := t.helpers.deps.UserMemoryRepo.Create(ctx, memory); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_failed")
		return "", fmt.Errorf("save user memory: %w", err)
	}

	span.SetAttributes(attribute.String("memory_id", memory.ID.String()))
	t.helpers.logger.Info(ctx, "save_user_memory.success", map[string]any{
		"user_id":    userID.String(),
		"memory_id":  memory.ID.String(),
		"category":   args.Category,
		"importance": args.Importance,
	})

	return formatUserMemorySaved(memory.ID.String(), args.Category, args.Importance, args.Title), nil
}

// =============================================================================
// update_user_memory
// =============================================================================

type updateUserMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createUpdateUserMemoryTool() tool.InvokableTool {
	return &updateUserMemoryTool{helpers: h}
}

func (t *updateUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "update_user_memory",
		Desc: updateUserMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"memory_id": {
				Type:     schema.String,
				Desc:     "ID of the memory to update",
				Required: true,
			},
			"title": {
				Type:     schema.String,
				Desc:     "New title",
				Required: false,
			},
			"content": {
				Type:     schema.String,
				Desc:     "New content. For feedback/project: must include '**Why:**' and '**How to apply:**' sections",
				Required: false,
			},
			"description": {
				Type:     schema.String,
				Desc:     "New one-line description",
				Required: false,
			},
			"importance": {
				Type:     schema.String,
				Desc:     "New importance level: low, medium, high, critical",
				Required: false,
				Enum:     model.ValidImportances,
			},
			"tags": {
				Type:     schema.Array,
				Desc:     "New tags (replaces existing)",
				Required: false,
			},
			"metadata": {
				Type:     schema.Object,
				Desc:     "New metadata (replaces existing)",
				Required: false,
			},
		}),
	}, nil
}

func (t *updateUserMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.update_user_memory",
		trace.WithAttributes(
			attribute.String("turn_id", ""),
		),
	)
	defer span.End()

	var args struct {
		MemoryID    string         `json:"memory_id"`
		Title       *string        `json:"title,omitempty"`
		Content     *string        `json:"content,omitempty"`
		Description *string        `json:"description,omitempty"`
		Importance  *string        `json:"importance,omitempty"`
		Tags        []string       `json:"tags,omitempty"`
		Metadata    map[string]any `json:"metadata,omitempty"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "update_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	if args.MemoryID == "" {
		span.SetStatus(codes.Error, "missing_memory_id")
		return "", fmt.Errorf("memory_id is required")
	}
	span.SetAttributes(attribute.String("memory_id", args.MemoryID))

	memoryID, err := uuid.Parse(args.MemoryID)
	if err != nil {
		span.SetStatus(codes.Error, "invalid_memory_id")
		return "", fmt.Errorf("invalid memory_id: %w", err)
	}

	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_user_id_failed")
		return "", fmt.Errorf("get user ID: %w", err)
	}
	span.SetAttributes(attribute.String("user_id", userID.String()))

	existing, err := t.helpers.deps.UserMemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_failed")
		return "", fmt.Errorf("get memory: %w", err)
	}
	if existing.UserID != userID {
		span.SetStatus(codes.Error, "not_owner")
		return "", fmt.Errorf("memory %s does not belong to current user", args.MemoryID)
	}

	if validationErr := validateMemoryUpdateArgs(args, existing); validationErr != nil {
		span.SetStatus(codes.Error, "validation_failed")
		return "", validationErr
	}

	fields := buildMemoryUpdateFields(args)
	if len(fields) == 0 {
		span.SetAttributes(attribute.Bool("no_fields", true))
		return "No fields to update.", nil
	}
	span.SetAttributes(attribute.Int("field_count", len(fields)))

	if err := t.helpers.deps.UserMemoryRepo.Update(ctx, memoryID, fields); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		return "", fmt.Errorf("update memory: %w", err)
	}

	return formatUserMemoryUpdated(args.MemoryID), nil
}

// =============================================================================
// delete_user_memory
// =============================================================================

type deleteUserMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createDeleteUserMemoryTool() tool.InvokableTool {
	return &deleteUserMemoryTool{helpers: h}
}

func (t *deleteUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "delete_user_memory",
		Desc: deleteUserMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"memory_id": {
				Type:     schema.String,
				Desc:     "ID of the memory to delete",
				Required: true,
			},
		}),
	}, nil
}

func (t *deleteUserMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.delete_user_memory",
		trace.WithAttributes(
			attribute.String("turn_id", ""),
		),
	)
	defer span.End()

	var args struct {
		MemoryID string `json:"memory_id"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "delete_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	if args.MemoryID == "" {
		span.SetStatus(codes.Error, "missing_memory_id")
		return "", fmt.Errorf("memory_id is required")
	}
	span.SetAttributes(attribute.String("memory_id", args.MemoryID))

	memoryID, err := uuid.Parse(args.MemoryID)
	if err != nil {
		span.SetStatus(codes.Error, "invalid_memory_id")
		return "", fmt.Errorf("invalid memory_id: %w", err)
	}

	// Verify ownership
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_user_id_failed")
		return "", fmt.Errorf("get user ID: %w", err)
	}
	span.SetAttributes(attribute.String("user_id", userID.String()))

	existing, err := t.helpers.deps.UserMemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_failed")
		return "", fmt.Errorf("get memory: %w", err)
	}
	if existing.UserID != userID {
		span.SetStatus(codes.Error, "not_owner")
		return "", fmt.Errorf("memory %s does not belong to current user", args.MemoryID)
	}

	if err := t.helpers.deps.UserMemoryRepo.Delete(ctx, memoryID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete_failed")
		return "", fmt.Errorf("delete memory: %w", err)
	}

	return formatUserMemoryDeleted(args.MemoryID), nil
}

// =============================================================================
// list_user_memory
// =============================================================================

type listUserMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createListUserMemoryTool() tool.InvokableTool {
	return &listUserMemoryTool{helpers: h}
}

func (t *listUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "list_user_memory",
		Desc: listUserMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"category": {
				Type:     schema.String,
				Desc:     "Optional: filter by category (user, feedback, project, reference)",
				Required: false,
				Enum:     model.ValidUserMemoryCategories,
			},
			"limit": {
				Type:     schema.Integer,
				Desc:     "Optional: maximum number of memories to return (default: 20)",
				Required: false,
			},
		}),
	}, nil
}

func (t *listUserMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.list_user_memory",
		trace.WithAttributes(
			attribute.String("turn_id", ""),
		),
	)
	defer span.End()

	var args struct {
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "list_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_user_id_failed")
		return "", fmt.Errorf("get user ID: %w", err)
	}
	span.SetAttributes(
		attribute.String("user_id", userID.String()),
		attribute.String("category", args.Category),
	)

	if args.Limit <= 0 {
		args.Limit = 20
	}
	span.SetAttributes(attribute.Int("limit", args.Limit))

	var memories []*model.UserMemory
	if args.Category != "" {
		if !model.IsValidUserMemoryCategory(args.Category) {
			span.SetStatus(codes.Error, "invalid_category")
			return "", fmt.Errorf("invalid category: %s", args.Category)
		}
		memories, err = t.helpers.deps.UserMemoryRepo.ListByCategory(ctx, userID, args.Category, args.Limit)
	} else {
		memories, err = t.helpers.deps.UserMemoryRepo.ListByUser(ctx, userID, args.Limit)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		return "", fmt.Errorf("list user memories: %w", err)
	}

	if len(memories) == 0 {
		span.SetAttributes(attribute.Int("count", 0))
		return formatNoUserMemories(), nil
	}

	items := make([]userMemoryItem, len(memories))
	for i, mem := range memories {
		items[i] = userMemoryItem{
			Index:       i + 1,
			Category:    mem.Category,
			Importance:  mem.Importance,
			Title:       mem.Title,
			Content:     stringutil.TruncateByRune(mem.Content, 200),
			ID:          mem.ID.String(),
			CreatedAt:   mem.CreatedAt.Format("2006-01-02"),
			AccessCount: mem.AccessCount,
		}
	}

	span.SetAttributes(attribute.Int("count", len(memories)))
	return formatUserMemoriesList(len(memories), items), nil
}

// =============================================================================
// Helper functions
// =============================================================================
