package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
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
	turnID  uuid.UUID // Turn ID for publishToolMessages
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

	// 1. Parse arguments using parseToolArgsWithPersist so parse errors are
	//    also persisted as tool_result (consistent with other tools).
	var args struct {
		Todos []model.TodoItem `json:"todos,omitempty"`
	}
	if ok, errMsg := parseToolArgsWithPersist(ctx, t.helper, t.session.ID,
		t.session.OwnerRefID, t.turnID, "todoWrite", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if len(args.Todos) == 0 {
		args.Todos = make([]model.TodoItem, 0)
	}
	span.SetAttributes(attribute.Int("todo_count", len(args.Todos)))

	// 2. Validate todos.
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

	// 3. Update session's todo_list.
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

	// 4. Publish update event to notify the frontend.
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

	// 5. Render complete TodoList as tool_result.
	fullList := formatTodoList(args.Todos)

	// 6. Persist toolcall_input + toolcall_output to DB (best-effort).
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helper,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "todoWrite",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      fullList,
	}); err != nil {
		span.RecordError(err)
		t.helper.logger.Warn(ctx, "todoWriteTool.publish_messages_failed", map[string]any{
			"session_id": t.session.ID.String(),
			"error":      err.Error(),
		})
		// Do not return error: todo was updated successfully, message
		// persistence failure is non-fatal.
	}

	// 7. Return full TodoList to LLM.
	return fullList, nil
}
