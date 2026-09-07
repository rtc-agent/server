package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
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
		Desc:        "List all running sub agent sessions (descendants of the current session tree). Returns an array of sub agent sessions with their session ID, title, status, and creation time. Use this to check the status of spawned sub agents.",
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
	resultJSON, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("list_sub_agent: marshal result: %w", err)
	}
	resultStr := string(resultJSON)

	// 4. Get tool_call_id.
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("list_sub_agent: tool_call_id not set in context")
	}

	turnUUID := t.turnID
	if turnUUID == uuid.Nil {
		return "", fmt.Errorf("list_sub_agent: turn UUID is nil")
	}

	// 5. Create toolcall_input + toolcall_output messages and publish.
	toolCallData := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: "list_sub_agent",
		Input:    argumentsInJSON,
	}
	inputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: toolCallData,
	}

	completedStatus := "completed"
	outputToolCall := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: "list_sub_agent",
		Input:    argumentsInJSON,
		Output:   &resultStr,
		Status:   &completedStatus,
	}
	outputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: outputToolCall,
	}

	var inputMsgID, outputMsgID uuid.UUID

	_, err = t.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		// Create toolcall_input message.
		inputMsg, createErr := primitives.CreateMessage(
			txCtx, t.helpers.deps,
			t.session.ID, &turnUUID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			inputContent,
			protocol.MessageStreamingCompleted,
			"",  // system-generated
			nil, // no parent
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_input: %w", createErr)
		}
		inputMsgID = inputMsg.ID

		// Create toolcall_output message (parent = toolcall_input).
		outputMsg, createErr := primitives.CreateMessage(
			txCtx, t.helpers.deps,
			t.session.ID, &turnUUID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			outputContent,
			protocol.MessageStreamingCompleted,
			"",          // system-generated
			&inputMsgID, // parent points to toolcall_input
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_output: %w", createErr)
		}
		outputMsgID = outputMsg.ID

		// Build publish items.
		ch := channel.UserTopic(t.session.OwnerRefID)
		updateItems := []updates.UpdatePublishItem{
			{
				Channel: ch,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(inputMsgID.String()),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(outputMsgID.String()),
					},
				},
			},
		}
		return updateItems, nil
	})
	if err != nil {
		return "", fmt.Errorf("list_sub_agent: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "listSubAgent.completed", map[string]any{
		"session_id":    t.session.ID.String(),
		"root_session":  rootSessionID.String(),
		"count":         len(items),
		"input_msg_id":  inputMsgID.String(),
		"output_msg_id": outputMsgID.String(),
	})

	return resultStr, nil
}
