package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// memoryToolBase holds the shared fields and methods for all memory tools.
// Embedding this struct eliminates duplicated persistError/persistResult boilerplate.
type memoryToolBase struct {
	session  *model.Session
	helpers  *helpers
	turnID   uuid.UUID
	toolName string
}

// persistError persists an error message to DB for cache consistency.
func (b *memoryToolBase) persistError(ctx context.Context, argumentsInJSON, errMsg string) {
	persistToolError(ctx, b.helpers, b.session, b.turnID, b.toolName, argumentsInJSON, errMsg)
}

// persistResult persists a success result to DB for cache consistency.
func (b *memoryToolBase) persistResult(ctx context.Context, argumentsInJSON, resultJSON string) {
	persistToolResult(ctx, b.helpers, b.session, b.turnID, b.toolName, argumentsInJSON, resultJSON)
}

// =============================================================================
// saveMemory
// =============================================================================

type saveMemoryTool struct{ memoryToolBase }

func (h *helpers) createSaveMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &saveMemoryTool{memoryToolBase{session: session, helpers: h, turnID: turnID, toolName: "saveMemory"}}
}

func (t *saveMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "saveMemory",
		Desc: saveMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"category": {
				Type:     schema.String,
				Desc:     "Category: user, feedback, project, or reference",
				Required: true,
				Enum:     []string{"user", "feedback", "project", "reference"},
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

func (t *saveMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.saveMemory",
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "saveMemory", argumentsInJSON, &args); !ok {
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
	if !memory.IsValidUserMemoryType(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category: %s (must be one of: %s)",
			args.Category, strings.Join(memory.ValidUserMemoryTypes, ", "))
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
	if args.Category == "feedback" || args.Category == "project" {
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

	// Build metadata with importance and source session (OKF sources format)
	metadataMap := args.Metadata
	if metadataMap == nil {
		metadataMap = make(map[string]any)
	}
	metadataMap["importance"] = args.Importance
	metadataMap["sources"] = []map[string]any{
		{
			"id":       fmt.Sprintf("session-%s", t.session.ID.String()[:8]),
			"resource": fmt.Sprintf("rtc-agent://session/%s", t.session.ID.String()),
		},
	}
	metadataJSON, _ := json.Marshal(metadataMap)

	descStr := ""
	if args.Description != nil {
		descStr = *args.Description
	}

	mem := &memory.Memory{
		Scope:       memory.ScopeUser,
		ScopeID:     userID,
		Type:        args.Category,
		Title:       args.Title,
		Content:     args.Content,
		Description: descStr,
		Tags:        memory.StringArray(args.Tags),
		TokenCount:  estimateMemoryTokens(args.Content),
		Timestamp:   time.Now(),
		Metadata:    memory.JSONBString(metadataJSON),
	}

	if err := t.helpers.deps.MemoryRepo.Create(ctx, mem); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "create_failed")
		errMsg := fmt.Sprintf("Error: save memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	span.SetAttributes(attribute.String("memory_id", mem.ID.String()))
	t.helpers.logger.Info(ctx, "saveMemory.success", map[string]any{
		"user_id":    userID.String(),
		"memory_id":  mem.ID.String(),
		"category":   args.Category,
		"importance": args.Importance,
	})

	resultJSON := formatMemorySaved(mem.ID.String(), args.Category, args.Importance, args.Title)
	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// =============================================================================
// updateMemory
// =============================================================================

type updateMemoryTool struct{ memoryToolBase }

func (h *helpers) createUpdateMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &updateMemoryTool{memoryToolBase{session: session, helpers: h, turnID: turnID, toolName: "updateMemory"}}
}

func (t *updateMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "updateMemory",
		Desc: updateMemoryDesc,
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

func (t *updateMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.updateMemory",
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "updateMemory", argumentsInJSON, &args); !ok {
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

	existing, err := t.helpers.deps.MemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_failed")
		errMsg := fmt.Sprintf("Error: get memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if existing.Scope != memory.ScopeUser {
		span.SetStatus(codes.Error, "wrong_scope")
		errMsg := fmt.Sprintf("Error: memory %s not found", args.MemoryID)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if existing.ScopeID != userID {
		span.SetStatus(codes.Error, "not_owner")
		errMsg := fmt.Sprintf("Error: memory %s not found", args.MemoryID)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	if validationErr := validateMemoryUpdateArgs(args, existing); validationErr != nil {
		span.SetStatus(codes.Error, "validation_failed")
		errMsg := fmt.Sprintf("Error: %v", validationErr)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	fields, err := buildMemoryUpdateFields(args)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_metadata_failed")
		errMsg := fmt.Sprintf("Error: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if len(fields) == 0 {
		span.SetAttributes(attribute.Bool("no_fields", true))
		resultJSON := "No fields to update."
		t.persistResult(ctx, argumentsInJSON, resultJSON)
		return resultJSON, nil
	}
	span.SetAttributes(attribute.Int("field_count", len(fields)))

	if err := t.helpers.deps.MemoryRepo.Update(ctx, memoryID, fields); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		errMsg := fmt.Sprintf("Error: update memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	resultJSON := formatMemoryUpdated(args.MemoryID)
	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// =============================================================================
// deleteMemory
// =============================================================================

type deleteMemoryTool struct{ memoryToolBase }

func (h *helpers) createDeleteMemoryTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &deleteMemoryTool{memoryToolBase{session: session, helpers: h, turnID: turnID, toolName: "deleteMemory"}}
}

func (t *deleteMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "deleteMemory",
		Desc: deleteMemoryDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"memory_id": {
				Type:     schema.String,
				Desc:     "ID of the memory to delete",
				Required: true,
			},
		}),
	}, nil
}

func (t *deleteMemoryTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.deleteMemory",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	var args struct {
		MemoryID string `json:"memory_id"`
	}
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "deleteMemory", argumentsInJSON, &args); !ok {
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

	existing, err := t.helpers.deps.MemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "get_failed")
		errMsg := fmt.Sprintf("Error: get memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if existing.Scope != memory.ScopeUser {
		span.SetStatus(codes.Error, "wrong_scope")
		errMsg := fmt.Sprintf("Error: memory %s not found", args.MemoryID)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}
	if existing.ScopeID != userID {
		span.SetStatus(codes.Error, "not_owner")
		errMsg := fmt.Sprintf("Error: memory %s not found", args.MemoryID)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	if err := t.helpers.deps.MemoryRepo.Delete(ctx, memoryID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete_failed")
		errMsg := fmt.Sprintf("Error: delete memory: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	resultJSON := formatMemoryDeleted(args.MemoryID)
	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// =============================================================================
// listMemories
// =============================================================================

type listMemoriesTool struct{ memoryToolBase }

func (h *helpers) createListMemoriesTool(session *model.Session, turnID uuid.UUID) tool.InvokableTool {
	return &listMemoriesTool{memoryToolBase{session: session, helpers: h, turnID: turnID, toolName: "listMemories"}}
}

func (t *listMemoriesTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "listMemories",
		Desc: listMemoriesDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"category": {
				Type:     schema.String,
				Desc:     "Optional: filter by category (user, feedback, project, reference)",
				Required: false,
				Enum:     []string{"user", "feedback", "project", "reference"},
			},
			"limit": {
				Type:     schema.Integer,
				Desc:     "Optional: maximum number of memories to return (default: 20)",
				Required: false,
			},
		}),
	}, nil
}

func (t *listMemoriesTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helpers.tracer.Start(ctx, "tool.listMemories",
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
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helpers, t.session.ID, t.session.OwnerRefID, t.turnID, "listMemories", argumentsInJSON, &args); !ok {
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
	if args.Category != "" && !memory.IsValidUserMemoryType(args.Category) {
		span.SetStatus(codes.Error, "invalid_category")
		errMsg := fmt.Sprintf("Error: invalid category: %s", args.Category)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	var memories []*memory.Memory
	if args.Category != "" {
		memories, err = t.helpers.deps.MemoryRepo.ListByScope(ctx, memory.ScopeUser, userID, memory.ListOptions{Type: args.Category, Limit: args.Limit})
	} else {
		memories, err = t.helpers.deps.MemoryRepo.ListByScope(ctx, memory.ScopeUser, userID, memory.ListOptions{Limit: args.Limit})
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_failed")
		errMsg := fmt.Sprintf("Error: list memories: %v", err)
		t.persistError(ctx, argumentsInJSON, errMsg)
		return errMsg, nil
	}

	var resultJSON string
	if len(memories) == 0 {
		span.SetAttributes(attribute.Int("count", 0))
		resultJSON = formatNoMemories()
	} else {
		items := make([]memoryListItem, len(memories))
		for i, mem := range memories {
			items[i] = memoryListItem{
				Index:      i + 1,
				Category:   mem.Type,
				Importance: extractImportanceFromMetadata(mem.Metadata),
				Title:      mem.Title,
				Content:    stringutil.TruncateByRune(mem.Content, 200),
				ID:         mem.ID.String(),
				CreatedAt:  mem.CreatedAt.Format("2006-01-02"),
			}
		}

		span.SetAttributes(attribute.Int("count", len(memories)))
		resultJSON = formatMemoriesList(len(memories), items)
	}

	t.persistResult(ctx, argumentsInJSON, resultJSON)

	return resultJSON, nil
}

// =============================================================================
// Helper functions
// =============================================================================
