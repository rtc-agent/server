package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// TodoListAttachment injects the current todo list into LLM context.
//
// The todo list is stored in the Session model and is updated by the LLM
// via the todo_write tool. This attachment ensures the LLM always knows
// the current task state.
type TodoListAttachment struct {
	helpers *helpers
}

// NewTodoListAttachment creates a new TodoListAttachment.
func NewTodoListAttachment(h *helpers) *TodoListAttachment {
	return &TodoListAttachment{helpers: h}
}

// Name returns the attachment name.
func (a *TodoListAttachment) Name() string {
	return "TodoList"
}

// Build generates the todo list content for injection.
//
// Returns empty string if:
// - The session has no todo list
// - The todo list is empty
//
// The output is formatted as Markdown with tasks grouped by status:
// - In Progress (most important, shown first)
// - Pending
// - Completed (shown last, with strikethrough)
func (a *TodoListAttachment) Build(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (string, error) {
	session, err := a.helpers.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	if len(session.TodoList) == 0 {
		return "", nil
	}

	// Group tasks by status
	var inProgress, pending, completed []model.TodoItem
	for _, item := range session.TodoList {
		switch item.Status {
		case "in_progress":
			inProgress = append(inProgress, item)
		case "pending":
			pending = append(pending, item)
		case "completed":
			completed = append(completed, item)
		}
	}

	// Build the output
	var sb strings.Builder
	sb.WriteString("## Current Tasks\n\n")

	if len(inProgress) > 0 {
		sb.WriteString("### In Progress\n")
		for _, item := range inProgress {
			fmt.Fprintf(&sb, "- **%s**\n", item.Content)
		}
		sb.WriteString("\n")
	}

	if len(pending) > 0 {
		sb.WriteString("### Pending\n")
		for _, item := range pending {
			fmt.Fprintf(&sb, "- %s\n", item.Content)
		}
		sb.WriteString("\n")
	}

	if len(completed) > 0 {
		sb.WriteString("### Completed\n")
		for _, item := range completed {
			fmt.Fprintf(&sb, "- ~~%s~~\n", item.Content)
		}
		sb.WriteString("\n")
	}

	return sb.String(), nil
}
