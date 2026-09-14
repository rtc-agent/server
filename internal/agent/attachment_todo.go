package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
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

	return formatTodoList(session.TodoList), nil
}
