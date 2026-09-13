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
)

// todoWriteTool 实现 Claude Code 风格的 TodoWrite 工具
// 单工具 + 全量替换模式，更新后通过 publish update 通知前端
type todoWriteTool struct {
	helper  *helpers
	session *model.Session
}

func (t *todoWriteTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "todo_write",
		Desc: "Update the todo list for the current session. Replaces the entire list. Use proactively to track progress and pending tasks. Make sure that at least one task is in_progress at all times. Always provide both content (imperative) and active_form (present continuous) for each task.",
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
	// 解析参数
	var args struct {
		Todos []model.TodoItem `json:"todos,omitempty"`
	}
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("parse todos: %w", err)
	}

	if len(args.Todos) == 0 {
		args.Todos = make([]model.TodoItem, 0)
	}

	// 验证 todos
	for i, todo := range args.Todos {
		if todo.Content == "" {
			return "", fmt.Errorf("todo[%d].content is required but was empty. Ensure all required fields (content, status, active_form) are present with correct snake_case names", i)
		}
		if todo.ActiveForm == "" {
			return "", fmt.Errorf("todo[%d].active_form is required but was empty. Make sure you use 'active_form' (snake_case), not 'activeForm'", i)
		}
		if todo.Status != "pending" && todo.Status != "in_progress" && todo.Status != "completed" {
			return "", fmt.Errorf("todo[%d].status must be one of: pending, in_progress, completed. Got: %q", i, todo.Status)
		}
	}

	// 更新 session 的 todo_list
	todoList := model.JSONB[model.TodoItem](args.Todos)
	err := t.helper.deps.SessionRepo.UpdateFieldsActive(ctx, t.session.ID, map[string]any{
		"todo_list": todoList,
	})
	if err != nil {
		return "", fmt.Errorf("update todo_list: %w", err)
	}

	// 发布 update 事件通知前端
	_, err = t.helper.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		return primitives.BuildSessionUpdatedUpdates(t.session), nil
	})
	if err != nil {
		// 日志记录但不返回错误（\t\o\d\o 已更新成功）
		t.helper.logIfEnabled(ctx, "todoWriteTool.publish_update", map[string]any{
			"session_id": t.session.ID.String(),
			"error":      err.Error(),
		})
	}

	// 返回给 LLM 的结果（不包含在 transcript 中）
	return "<notification>Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable.</notification>", nil
}
