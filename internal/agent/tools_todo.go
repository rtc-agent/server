package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// todoWriteTool implements a Claude Code-style TodoWrite tool.
// Single tool + full replacement mode; publishes update events to notify the frontend after updates.
type todoWriteTool struct {
	helper  *helpers
	session *model.Session
}

func (t *todoWriteTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "todoWrite",
		Desc: todoWriteDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"todos": {
				Type:     schema.Array,
				Desc:     "Array of todo items (complete replacement)",
				Required: false,
				ElemInfo: &schema.ParameterInfo{
					Type: schema.Object,
					SubParams: map[string]*schema.ParameterInfo{
						"content": {
							Type:     schema.String,
							Desc:     "Task description (imperative)",
							Required: true,
						},
						"status": {
							Type:     schema.String,
							Desc:     "Task status",
							Enum:     []string{"pending", "in_progress", "completed"},
							Required: true,
						},
						"active_form": {
							Type:     schema.String,
							Desc:     "Task in progress description (present continuous)",
							Required: true,
						},
					},
				},
			},
		}),
	}, nil
}

func (t *todoWriteTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	ctx, span := t.helper.tracer.Start(ctx, "tool.todoWrite",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.Int("args_length", len(argumentsInJSON)),
		),
	)
	defer span.End()

	// Parse arguments.
	var args struct {
		Todos []model.TodoItem `json:"todos,omitempty"`
	}
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse_failed")
		return "", fmt.Errorf("parse todos: %w", err)
	}

	if len(args.Todos) == 0 {
		args.Todos = make([]model.TodoItem, 0)
	}
	span.SetAttributes(attribute.Int("todo_count", len(args.Todos)))

	// Validate todos.
	for i, todo := range args.Todos {
		if todo.Content == "" {
			span.SetStatus(codes.Error, "missing_content")
			return "", fmt.Errorf("todo[%d].content is required but was empty. Ensure all required fields (content, status, active_form) are present with correct snake_case names", i)
		}
		if todo.ActiveForm == "" {
			span.SetStatus(codes.Error, "missing_active_form")
			return "", fmt.Errorf("todo[%d].active_form is required but was empty. Make sure you use 'active_form' (snake_case), not 'activeForm'", i)
		}
		if todo.Status != "pending" && todo.Status != "in_progress" && todo.Status != "completed" {
			span.SetStatus(codes.Error, "invalid_status")
			return "", fmt.Errorf("todo[%d].status must be one of: pending, in_progress, completed. Got: %q", i, todo.Status)
		}
	}

	// Update session's todo_list.
	todoList := model.JSONB[model.TodoItem](args.Todos)
	err := t.helper.deps.SessionRepo.UpdateFieldsActive(ctx, t.session.ID, map[string]any{
		"todo_list": todoList,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update_failed")
		return "", fmt.Errorf("update todo_list: %w", err)
	}

	// Update in-memory session object to ensure published events reflect
	// the latest state. Without this, the frontend would receive stale
	// todo_list data and flash back to the old state.
	t.session.TodoList = todoList

	// Publish update event to notify the frontend.
	_, err = t.helper.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		return primitives.BuildSessionUpdatedUpdates(t.session), nil
	})
	if err != nil {
		// Log but do not return error (todo was updated successfully).
		span.RecordError(err)
		t.helper.logger.Info(ctx, "todoWriteTool.publish_update", map[string]any{
			"session_id": t.session.ID.String(),
			"error":      err.Error(),
		})
	}

	// Return result to LLM (not included in transcript).
	return formatTodoNotification(), nil
}
