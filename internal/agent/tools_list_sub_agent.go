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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// listSubAgentTool lists all running (active) sub agent sessions
// that are descendants of the current session tree.
//
// Unlike subAgent tool, this tool does NOT interrupt the turn.
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
		Name:        "listSubAgent",
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
	ctx, span := t.helpers.tracer.Start(ctx, "tool.listSubAgent",
		trace.WithAttributes(
			attribute.String("session_id", t.session.ID.String()),
			attribute.String("turn_id", t.turnID.String()),
		),
	)
	defer span.End()

	// 1. Determine root session ID.
	rootSessionID := t.session.ID
	if t.session.RootServerSessionID != uuid.Nil {
		rootSessionID = t.session.RootServerSessionID
	}
	span.SetAttributes(attribute.String("root_session_id", rootSessionID.String()))

	// 2. Query all active descendant sessions.
	descendants, err := t.helpers.deps.SessionRepo.ListByRoot(ctx, rootSessionID, string(protocol.SessionStatusActive))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list_by_root_failed")
		return "", fmt.Errorf("listSubAgent: list by root: %w", err)
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal_failed")
		return "", fmt.Errorf("listSubAgent: marshal result: %w", err)
	}

	// 4. Publish toolcall_input + toolcall_output messages.
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         t.helpers,
		SessionID:       t.session.ID,
		OwnerRefID:      t.session.OwnerRefID,
		TurnID:          t.turnID,
		ToolName:        "listSubAgent",
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish_failed")
		return "", fmt.Errorf("listSubAgent: publish messages: %w", err)
	}

	span.SetAttributes(attribute.Int("count", len(items)))
	t.helpers.logger.Info(ctx, "listSubAgent.completed", map[string]any{
		"session_id":   t.session.ID.String(),
		"root_session": rootSessionID.String(),
		"count":        len(items),
	})

	return resultJSON, nil
}
