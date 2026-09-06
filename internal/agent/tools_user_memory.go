package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// getUserIDFromContext 从上下文中获取用户 ID
// 通过查找当前 session 的 owner 来获取
func (h *helpers) getUserIDFromContext(ctx context.Context) (uuid.UUID, error) {
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("no session ID in context")
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get session: %w", err)
	}

	// OwnerRefID 是用户 ID 的字符串表示
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
		Desc: "Save persistent user-level memory that persists across sessions. " +
			"Use this to remember user preferences, project details, feedback, and references. " +
			"Categories: user (about the user), feedback (how to work with user), project (ongoing work), reference (external resources). " +
			"For feedback and project categories, content MUST include '**Why:**' and '**How to apply:**' sections.",
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
	var args struct {
		Category    string         `json:"category"`
		Importance  string         `json:"importance"`
		Title       string         `json:"title"`
		Content     string         `json:"content"`
		Description *string        `json:"description,omitempty"`
		Tags        []string       `json:"tags,omitempty"`
		Metadata    model.JSONB[any] `json:"metadata,omitempty"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "save_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}
	if args.Category == "" || args.Title == "" || args.Content == "" {
		return "", fmt.Errorf("category, title, and content are required")
	}

	// Validate category
	if !model.IsValidUserMemoryCategory(args.Category) {
		return "", fmt.Errorf("invalid category: %s (must be one of: %s)",
			args.Category, strings.Join(model.ValidUserMemoryCategories, ", "))
	}

	// Validate importance
	if args.Importance == "" {
		args.Importance = model.ImportanceMedium
	}
	if !model.IsValidImportance(args.Importance) {
		return "", fmt.Errorf("invalid importance: %s (must be one of: %s)",
			args.Importance, strings.Join(model.ValidImportances, ", "))
	}

	// Tool-layer validation: feedback and project must include Why/How structure
	if args.Category == model.UserMemoryCategoryFeedback || args.Category == model.UserMemoryCategoryProject {
		if err := validateStructuredContent(args.Content); err != nil {
			return "", fmt.Errorf("content validation failed for %s category: %w", args.Category, err)
		}
	}

	// Get user ID from context
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("get user ID: %w", err)
	}

	// Get session ID for source tracking
	sessionID := getSessionIDFromContext(ctx)

	// Generate embedding if service is available
	var embedding model.VectorArray
	if t.helpers.deps.EmbeddingService != nil && t.helpers.deps.EmbeddingService.Dimension() > 0 {
		// Build embedding text: title + description + content
		embedText := args.Title
		if args.Description != nil {
			embedText += " " + *args.Description
		}
		embedText += " " + args.Content

		vec, embedErr := t.helpers.deps.EmbeddingService.GenerateEmbedding(ctx, embedText)
		if embedErr != nil {
			t.helpers.logIfEnabled(ctx, "save_user_memory.embedding_error", map[string]any{
				"error": embedErr.Error(),
			})
			// Continue without embedding - keyword search still works
		} else {
			embedding = model.VectorArray(vec)
		}
	}

	// Create memory
	memory := &model.UserMemory{
		UserID:          userID,
		Category:        args.Category,
		Importance:      args.Importance,
		Title:           args.Title,
		Content:         args.Content,
		Description:     args.Description,
		Tags:            model.StringArray(args.Tags),
		Embedding:       embedding,
		Metadata:        args.Metadata,
		SourceSessionID: &sessionID,
	}

	if err := t.helpers.deps.UserMemoryRepo.Create(ctx, memory); err != nil {
		return "", fmt.Errorf("save user memory: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "save_user_memory.success", map[string]any{
		"user_id":     userID.String(),
		"memory_id":   memory.ID.String(),
		"category":    args.Category,
		"importance":  args.Importance,
		"has_embedding": len(embedding) > 0,
	})

	return fmt.Sprintf("User memory saved successfully (ID: %s, Category: %s, Importance: %s, Title: %s)",
		memory.ID.String(), args.Category, args.Importance, args.Title), nil
}

// validateStructuredContent 验证 feedback/project 类型的内容结构
// 必须包含 **Why:** 和 **How to apply:** 段落
func validateStructuredContent(content string) error {
	contentLower := strings.ToLower(content)
	hasWhy := strings.Contains(contentLower, "**why:**") || strings.Contains(contentLower, "**why**")
	hasHow := strings.Contains(contentLower, "**how to apply:**") || strings.Contains(contentLower, "**how to apply**")

	if !hasWhy && !hasHow {
		return fmt.Errorf("content must include '**Why:** ...' and '**How to apply:** ...' sections")
	}
	if !hasWhy {
		return fmt.Errorf("content must include a '**Why:** ...' section explaining the reason")
	}
	if !hasHow {
		return fmt.Errorf("content must include a '**How to apply:** ...' section explaining how to apply it")
	}
	return nil
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
		Desc: "Update an existing user memory. " +
			"You can update title, content, description, tags, importance, or metadata. " +
			"Only the fields you provide will be updated.",
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
	var args struct {
		MemoryID    string         `json:"memory_id"`
		Title       *string        `json:"title,omitempty"`
		Content     *string        `json:"content,omitempty"`
		Description *string        `json:"description,omitempty"`
		Importance  *string        `json:"importance,omitempty"`
		Tags        []string       `json:"tags,omitempty"`
		Metadata    model.JSONB[any] `json:"metadata,omitempty"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "update_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	if args.MemoryID == "" {
		return "", fmt.Errorf("memory_id is required")
	}

	// Parse memory ID
	memoryID, err := uuid.Parse(args.MemoryID)
	if err != nil {
		return "", fmt.Errorf("invalid memory_id: %w", err)
	}

	// Get user ID and verify ownership
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("get user ID: %w", err)
	}

	// Fetch existing memory to verify ownership and category
	existing, err := t.helpers.deps.UserMemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		return "", fmt.Errorf("get memory: %w", err)
	}
	if existing.UserID != userID {
		return "", fmt.Errorf("memory %s does not belong to current user", args.MemoryID)
	}

	// Validate importance if provided
	if args.Importance != nil && !model.IsValidImportance(*args.Importance) {
		return "", fmt.Errorf("invalid importance: %s", *args.Importance)
	}

	// Validate content structure for feedback/project if content is being updated
	if args.Content != nil {
		if existing.Category == model.UserMemoryCategoryFeedback || existing.Category == model.UserMemoryCategoryProject {
			if err := validateStructuredContent(*args.Content); err != nil {
				return "", fmt.Errorf("content validation failed: %w", err)
			}
		}
	}

	// Build update fields
	fields := make(map[string]any)
	if args.Title != nil {
		fields["title"] = *args.Title
	}
	if args.Content != nil {
		fields["content"] = *args.Content
	}
	if args.Description != nil {
		fields["description"] = *args.Description
	}
	if args.Importance != nil {
		fields["importance"] = *args.Importance
	}
	if args.Tags != nil {
		fields["tags"] = model.StringArray(args.Tags)
	}
	if args.Metadata != nil {
		fields["metadata"] = args.Metadata
	}

	if len(fields) == 0 {
		return "No fields to update.", nil
	}

	// Re-generate embedding if content/description/title changed
	if args.Content != nil || args.Description != nil || args.Title != nil {
		if t.helpers.deps.EmbeddingService != nil && t.helpers.deps.EmbeddingService.Dimension() > 0 {
			title := existing.Title
			if args.Title != nil {
				title = *args.Title
			}
			description := ""
			if existing.Description != nil {
				description = *existing.Description
			}
			if args.Description != nil {
				description = *args.Description
			}
			content := existing.Content
			if args.Content != nil {
				content = *args.Content
			}

			embedText := title
			if description != "" {
				embedText += " " + description
			}
			embedText += " " + content

			if vec, embedErr := t.helpers.deps.EmbeddingService.GenerateEmbedding(ctx, embedText); embedErr == nil {
				fields["embedding"] = model.VectorArray(vec)
			} else {
				t.helpers.logIfEnabled(ctx, "update_user_memory.embedding_error", map[string]any{
					"error": embedErr.Error(),
				})
			}
		}
	}

	if err := t.helpers.deps.UserMemoryRepo.Update(ctx, memoryID, fields); err != nil {
		return "", fmt.Errorf("update memory: %w", err)
	}

	return fmt.Sprintf("User memory %s updated successfully.", args.MemoryID), nil
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
		Desc: "Soft-delete a user memory. The memory will be marked as deleted and won't appear in future queries.",
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
	var args struct {
		MemoryID string `json:"memory_id"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "delete_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	if args.MemoryID == "" {
		return "", fmt.Errorf("memory_id is required")
	}

	memoryID, err := uuid.Parse(args.MemoryID)
	if err != nil {
		return "", fmt.Errorf("invalid memory_id: %w", err)
	}

	// Verify ownership
	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("get user ID: %w", err)
	}

	existing, err := t.helpers.deps.UserMemoryRepo.GetByID(ctx, memoryID)
	if err != nil {
		return "", fmt.Errorf("get memory: %w", err)
	}
	if existing.UserID != userID {
		return "", fmt.Errorf("memory %s does not belong to current user", args.MemoryID)
	}

	if err := t.helpers.deps.UserMemoryRepo.Delete(ctx, memoryID); err != nil {
		return "", fmt.Errorf("delete memory: %w", err)
	}

	return fmt.Sprintf("User memory %s deleted successfully.", args.MemoryID), nil
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
		Desc: "List user memories. Optionally filter by category. " +
			"Shows title, category, importance, and a preview of content.",
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
	var args struct {
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "list_user_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	userID, err := t.helpers.getUserIDFromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("get user ID: %w", err)
	}

	if args.Limit <= 0 {
		args.Limit = 20
	}

	var memories []*model.UserMemory
	if args.Category != "" {
		if !model.IsValidUserMemoryCategory(args.Category) {
			return "", fmt.Errorf("invalid category: %s", args.Category)
		}
		memories, err = t.helpers.deps.UserMemoryRepo.ListByCategory(ctx, userID, args.Category, args.Limit)
	} else {
		memories, err = t.helpers.deps.UserMemoryRepo.ListByUser(ctx, userID, args.Limit)
	}
	if err != nil {
		return "", fmt.Errorf("list user memories: %w", err)
	}

	if len(memories) == 0 {
		return "No user memories found.", nil
	}

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d user memories:\n\n", len(memories))

	for i, mem := range memories {
		fmt.Fprintf(&output, "%d. **[%s/%s]** %s\n", i+1, mem.Category, mem.Importance, mem.Title)
		fmt.Fprintf(&output, "   %s\n", truncateString(mem.Content, 200))
		fmt.Fprintf(&output, "   ID: %s | Created: %s | Accesses: %d\n",
			mem.ID.String(), mem.CreatedAt.Format("2006-01-02"), mem.AccessCount)
		output.WriteString("\n")
	}

	return output.String(), nil
}

// =============================================================================
// Helper functions
// =============================================================================

func init() {
	// Ensure logger is available (suppress unused import if only used conditionally)
	_ = zap.Error
	_ = logger.Info
}
