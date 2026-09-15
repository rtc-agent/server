package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// listSubAgentTool lists all running (active) sub agent sessions
// that are descendants of the current session tree.
//
// Unlike sub_agent tool, this tool does NOT interrupt the turn.
// It creates two messages (toolcall_input + toolcall_output) and
// publishes them in a single transaction, then returns the result
// directly to the LLM.
type listSubAgentTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

func (t *listSubAgentTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        "list_sub_agent",
		Desc:        listSubAgentDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

// listSubAgentItem is a single entry in the tool result.
type listSubAgentItem struct {
	SubSessionID string `json:"sub_session_id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func (t *listSubAgentTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// 1. Determine root session ID.
	rootSessionID := t.session.ID
	if t.session.RootServerSessionID != uuid.Nil {
		rootSessionID = t.session.RootServerSessionID
	}

	// 2. Query all active descendant sessions.
	descendants, err := t.helpers.deps.SessionRepo.ListByRoot(ctx, rootSessionID, string(protocol.SessionStatusActive))
	if err != nil {
		return "", fmt.Errorf("list_sub_agent: list by root: %w", err)
	}

	// 3. Build result JSON.
	items := make([]listSubAgentItem, 0, len(descendants))
	for _, s := range descendants {
		items = append(items, listSubAgentItem{
			SubSessionID: s.ID.String(),
			Title:        s.Title,
			Status:       s.Status,
			CreatedAt:    s.CreatedAt.Format(time.RFC3339),
			UpdatedAt:    s.UpdatedAt.Format(time.RFC3339),
		})
	}
	resultJSON, err := mustMarshalJSON(items)
	if err != nil {
		return "", fmt.Errorf("list_sub_agent: marshal result: %w", err)
	}

	// 4. Publish toolcall_input + toolcall_output messages.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "list_sub_agent",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		return "", fmt.Errorf("list_sub_agent: publish messages: %w", err)
	}

	t.helpers.logger.Info(ctx, "listSubAgent.completed", map[string]any{
		"session_id":   t.session.ID.String(),
		"root_session": rootSessionID.String(),
		"count":        len(items),
	})

	return resultJSON, nil
}
