package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// saveSessionMemoryTool 保存会话记忆
type saveSessionMemoryTool struct {
	helpers *helpers
}

func (h *helpers) createSaveSessionMemoryTool() tool.InvokableTool {
	return &saveSessionMemoryTool{helpers: h}
}

func (t *saveSessionMemoryTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "save_session_memory",
		Desc: "Save important information from the current conversation to session memory. " +
			"Use this to remember key decisions, progress, issues, and learnings throughout the session. " +
			"These memories will be used for context compression and can be retrieved later.",
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
	var args struct {
		Category string         `json:"category"`
		Title    string         `json:"title"`
		Content  string         `json:"content"`
		Metadata model.JSONB[any] `json:"metadata,omitempty"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "save_session_memory", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	// Extract session ID from context
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return "", fmt.Errorf("no session ID in context")
	}

	// Validate required fields
	if args.Category == "" || args.Title == "" || args.Content == "" {
		return "", fmt.Errorf("category, title, and content are required")
	}

	// Validate category
	if !model.IsValidCategory(args.Category) {
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
		Metadata:   args.Metadata,
		TokenCount: &tokenCount,
	}

	// Save to database
	if err := t.helpers.deps.SessionMemoryRepo.Create(ctx, memory); err != nil {
		return "", fmt.Errorf("save memory: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "save_session_memory.success", map[string]any{
		"session_id":  sessionID.String(),
		"memory_id":   memory.ID.String(),
		"category":    args.Category,
		"token_count": tokenCount,
	})

	return fmt.Sprintf("Session memory saved successfully (ID: %s, Category: %s, Title: %s)",
		memory.ID.String(), args.Category, args.Title), nil
}

// listSessionMemoriesTool 列出会话记忆
type listSessionMemoriesTool struct {
	helpers *helpers
}

func (h *helpers) createListSessionMemoriesTool() tool.InvokableTool {
	return &listSessionMemoriesTool{helpers: h}
}

func (t *listSessionMemoriesTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "list_session_memories",
		Desc: "List all session memories for the current session. " +
			"Use this to review what has been remembered so far.",
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
	var args struct {
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if ok, msg := parseToolArgs(ctx, t.helpers, "list_session_memories", argumentsInJSON, &args); !ok {
		return msg, nil
	}

	// Extract session ID from context
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return "", fmt.Errorf("no session ID in context")
	}

	// Set default limit
	if args.Limit <= 0 {
		args.Limit = 20
	}

	// Query memories
	var memories []*model.SessionMemory
	var err error
	if args.Category != "" {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListByCategory(ctx, sessionID, args.Category, args.Limit)
	} else {
		memories, err = t.helpers.deps.SessionMemoryRepo.ListBySession(ctx, sessionID, args.Limit)
	}
	if err != nil {
		return "", fmt.Errorf("list memories: %w", err)
	}

	if len(memories) == 0 {
		return "No session memories found.", nil
	}

	// Format output
	var output string
	output += fmt.Sprintf("Found %d session memories:\n\n", len(memories))

	for i, mem := range memories {
		output += fmt.Sprintf("%d. **[%s]** %s\n", i+1, mem.Category, mem.Title)
		output += fmt.Sprintf("   %s\n", truncateString(mem.Content, 200))
		if !mem.CreatedAt.IsZero() {
			output += fmt.Sprintf("   Created: %s\n", mem.CreatedAt.Format("2006-01-02 15:04"))
		}
		output += "\n"
	}

	return output, nil
}
