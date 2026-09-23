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
// saveUserMemory
// =============================================================================

type saveUserMemoryTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createSaveUserMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &saveUserMemoryTool{session: session, helpers: h, turnID: turnID}
}

func (t *saveUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "saveUserMemory",
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
	ctx, span := t.helpers.tracer.Start(ctx, "tool.saveUserMemory",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "saveUserMemory", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	// Validate required fields
	if args.Category == "" || args.Title == "" || args.Content == "" {
		span.SetStatus(codes.Error, "missing_required_fields")
		errMsg := "Error: category, title, and content are required"
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(
		attribute.String("category", args.Category),
		attribute.String("importance", args.Importance),
		attribute.Int("content_length", len(args.Content)),
	)

	// Validate category
	if !model.IsValidUserMemoryCategory(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category: %s (must be one of: %s)",
			args.Category, strings.Join(model.ValidUserMemoryCategories, ", "))
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	// Validate importance
	if args.Importance == "" {
		args.Importance = model.ImportanceMedium
	}
	if !model.IsValidImportance(args.Importance) {
		span.SetStatus(codes.Error, "invalid_importance")
		errMsg := fmt.Sprintf("Error: invalid importance: %s (must be one of: %s)",
			args.Importance, strings.Join(model.ValidImportances, ", "))
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	// Tool-layer validation: feedback and project must include Why/How structure
	if args.Category == model.UserMemoryCategoryFeedback || args.Category == model.UserMemoryCategoryProject {
		if err := validateStructuredContent(args.Content); err != nil {
			span.SetStatus(codes.Error, "content_validation_failed")
			errMsg := fmt.Sprintf("Error: content validation failed for %s category: %v", args.Category, err)
			t.persistError(ctx, argumentsInJSON, errMsg)
			return errMsg, nil
		}
	}

	// Get user ID from session
	userID, err := uuid.Parse(t.session.OwnerRefID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse_user_id_failed")
		errMsg := fmt.Sprintf("Error: parse user ID: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(attribute.String("user_id", userID.String()))

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
		SourceSessionID: &t.session.ID,
	}

	if err := t.helpers.deps.UserMemoryRepo.Create(ctx, memory); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_failed")
		errMsg := fmt.Sprintf("Error: save user memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	span.SetAttributes(attribute.String("memory_id", memory.ID.String()))
	t.helpers.logger.Info(ctx, "saveUserMemory.success", map[string]any{
		"user_id":    userID.String(),
		"memory_id":  memory.ID.String(),
		"category":   args.Category,
		"importance": args.Importance,
	})

	resultJSON := formatUserMemorySaved(memory.ID.String(), args.Category, args.Importance, args.Title)
	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// persistError persists an error message to DB for cache consistency.
func (t *saveUserMemoryTool) persistError(ctx context.Context, argumentsInJSON, errMsg string) {
	if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "saveUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      errMsg,
	}); publishErr != nil {
		t.helpers.logger.Warn(ctx, "saveUserMemory.persist_error_failed", map[string]any{
			"error": publishErr.Error(),
		})
	}
}

// persistResult persists a success result to DB for cache consistency.
func (t *saveUserMemoryTool) persistResult(ctx context.Context, argumentsInJSON, resultJSON string) {
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "saveUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		t.helpers.logger.Warn(ctx, "saveUserMemory.publish_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// =============================================================================
// updateUserMemory
// =============================================================================

type updateUserMemoryTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createUpdateUserMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &updateUserMemoryTool{session: session, helpers: h, turnID: turnID}
}

func (t *updateUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "updateUserMemory",
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
	ctx, span := t.helpers.tracer.Start(ctx, "tool.updateUserMemory",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "updateUserMemory", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.MemoryID == "" {
		span.SetStatus(codes.Error, "missing_memory_id")
		errMsg := "Error: memory_id is required"
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(attribute.String("memory_id", args.MemoryID))

	memoryID, err := uuid.Parse(args.MemoryID)
	if err != nil {
		span.SetStatus(codes.Error, "invalid_memory_id")
		errMsg := fmt.Sprintf("Error: invalid memory_id: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	// Get user ID from session
	userID, err := uuid.Parse(t.session.OwnerRefID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse_user_id_failed")
		errMsg := fmt.Sprintf("Error: parse user ID: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(attribute.String("user_id", userID.String()))

	existing, err := t.helpers.deps.UserMemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_failed")
		errMsg := fmt.Sprintf("Error: get memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if existing.UserID != userID {
		span.SetStatus(codes.Error, "not_owner")
		errMsg := fmt.Sprintf("Error: memory %s does not belong to current user", args.MemoryID)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	if validationErr := validateMemoryUpdateArgs(args, existing); validationErr != nil {
		span.SetStatus(codes.Error, "validation_failed")
		errMsg := fmt.Sprintf("Error: %v", validationErr)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	fields := buildMemoryUpdateFields(args)
	if len(fields) == 0 {
		span.SetAttributes(attribute.Bool("no_fields", true))
		resultJSON := "No fields to update."
		t.persistResult(ctx, argumentsInJSON, resultJSON)
		return resultJSON, nil
	}
	span.SetAttributes(attribute.Int("field_count", len(fields)))

	if err := t.helpers.deps.UserMemoryRepo.Update(ctx, memoryID, fields); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		errMsg := fmt.Sprintf("Error: update memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	resultJSON := formatUserMemoryUpdated(args.MemoryID)
	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// persistError persists an error message to DB for cache consistency.
func (t *updateUserMemoryTool) persistError(ctx context.Context, argumentsInJSON, errMsg string) {
	if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "updateUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      errMsg,
	}); publishErr != nil {
		t.helpers.logger.Warn(ctx, "updateUserMemory.persist_error_failed", map[string]any{
			"error": publishErr.Error(),
		})
	}
}

// persistResult persists a success result to DB for cache consistency.
func (t *updateUserMemoryTool) persistResult(ctx context.Context, argumentsInJSON, resultJSON string) {
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "updateUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		t.helpers.logger.Warn(ctx, "updateUserMemory.publish_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// =============================================================================
// deleteUserMemory
// =============================================================================

type deleteUserMemoryTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createDeleteUserMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &deleteUserMemoryTool{session: session, helpers: h, turnID: turnID}
}

func (t *deleteUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "deleteUserMemory",
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
	ctx, span := t.helpers.tracer.Start(ctx, "tool.deleteUserMemory",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	var args struct {
		MemoryID string `json:"memory_id"`
	}
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "deleteUserMemory", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.MemoryID == "" {
		span.SetStatus(codes.Error, "missing_memory_id")
		errMsg := "Error: memory_id is required"
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(attribute.String("memory_id", args.MemoryID))

	memoryID, err := uuid.Parse(args.MemoryID)
	if err != nil {
		span.SetStatus(codes.Error, "invalid_memory_id")
		errMsg := fmt.Sprintf("Error: invalid memory_id: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	// Get user ID from session
	userID, err := uuid.Parse(t.session.OwnerRefID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse_user_id_failed")
		errMsg := fmt.Sprintf("Error: parse user ID: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(attribute.String("user_id", userID.String()))

	existing, err := t.helpers.deps.UserMemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_failed")
		errMsg := fmt.Sprintf("Error: get memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if existing.UserID != userID {
		span.SetStatus(codes.Error, "not_owner")
		errMsg := fmt.Sprintf("Error: memory %s does not belong to current user", args.MemoryID)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	if err := t.helpers.deps.UserMemoryRepo.Delete(ctx, memoryID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete_failed")
		errMsg := fmt.Sprintf("Error: delete memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	resultJSON := formatUserMemoryDeleted(args.MemoryID)
	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// persistError persists an error message to DB for cache consistency.
func (t *deleteUserMemoryTool) persistError(ctx context.Context, argumentsInJSON, errMsg string) {
	if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "deleteUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      errMsg,
	}); publishErr != nil {
		t.helpers.logger.Warn(ctx, "deleteUserMemory.persist_error_failed", map[string]any{
			"error": publishErr.Error(),
		})
	}
}

// persistResult persists a success result to DB for cache consistency.
func (t *deleteUserMemoryTool) persistResult(ctx context.Context, argumentsInJSON, resultJSON string) {
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "deleteUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		t.helpers.logger.Warn(ctx, "deleteUserMemory.publish_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// =============================================================================
// listUserMemory
// =============================================================================

type listUserMemoryTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (h *helpers) createListUserMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &listUserMemoryTool{session: session, helpers: h, turnID: turnID}
}

func (t *listUserMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "listUserMemory",
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
	ctx, span := t.helpers.tracer.Start(ctx, "tool.listUserMemory",
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "listUserMemory", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	// Get user ID from session
	userID, err := uuid.Parse(t.session.OwnerRefID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse_user_id_failed")
		errMsg := fmt.Sprintf("Error: parse user ID: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	span.SetAttributes(
		attribute.String("user_id", userID.String()),
		attribute.String("category", args.Category),
	)

	if args.Limit <= 0 {
		args.Limit = 20
	}
	span.SetAttributes(attribute.Int("limit", args.Limit))

	// Validate category if provided
	if args.Category != "" && !model.IsValidUserMemoryCategory(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category: %s", args.Category)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	var memories []*model.UserMemory
	if args.Category != "" {
		memories, err = t.helpers.deps.UserMemoryRepo.ListByCategory(ctx, userID, args.Category, args.Limit)
	} else {
		memories, err = t.helpers.deps.UserMemoryRepo.ListByUser(ctx, userID, args.Limit)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		errMsg := fmt.Sprintf("Error: list user memories: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	var resultJSON string
	if len(memories) == 0 {
		span.SetAttributes(attribute.Int("count", 0))
		resultJSON = formatNoUserMemories()
	} else {
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
		resultJSON = formatUserMemoriesList(len(memories), items)
	}

	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// persistError persists an error message to DB for cache consistency.
func (t *listUserMemoryTool) persistError(ctx context.Context, argumentsInJSON, errMsg string) {
	if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "listUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      errMsg,
	}); publishErr != nil {
		t.helpers.logger.Warn(ctx, "listUserMemory.persist_error_failed", map[string]any{
			"error": publishErr.Error(),
		})
	}
}

// persistResult persists a success result to DB for cache consistency.
func (t *listUserMemoryTool) persistResult(ctx context.Context, argumentsInJSON, resultJSON string) {
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "listUserMemory",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		t.helpers.logger.Warn(ctx, "listUserMemory.publish_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// =============================================================================
// Helper functions
// =============================================================================
