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

// getSubAgentMessageTool queries the last message of a specific sub agent session.
//
// Like listSubAgentTool, this tool does NOT interrupt the turn.
// It creates two messages (toolcall_input + toolcall_output) and
// publishes them in a single transaction, then returns the result
// directly to the LLM.
type getSubAgentMessageTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// getSubAgentMessageArgs is the input schema for the get_sub_agent_message tool.
type getSubAgentMessageArgs struct {
	SubSessionID string `json:"sub_session_id"`
}

// getSubAgentMessageResult is the tool result.
type getSubAgentMessageResult struct {
	SubSessionID     string  `json:"sub_session_id"`
	SubSessionTitle  string  `json:"sub_session_title"`
	SubSessionStatus string  `json:"sub_session_status"`
	MessageID        *string `json:"message_id"`
	Role             *string `json:"role"`
	Content          string  `json:"content"`
	CreatedAt        *string `json:"created_at"`
}

func (t *getSubAgentMessageTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "get_sub_agent_message",
		Desc: "Get the last message from a specific sub agent session. Use this to check the latest output or status of a sub agent. Returns the message content, role, and metadata.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"sub_session_id": {
				Type:     schema.String,
				Desc:     "The server-side UUID of the sub agent session to query. Use list_sub_agent to get available session IDs.",
				Required: true,
			},
		}),
	}, nil
}

func (t *getSubAgentMessageTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// 1. Parse arguments.
	var args getSubAgentMessageArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "get_sub_agent_message", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.SubSessionID == "" {
		return "Error: sub_session_id is required", nil
	}

	subSessionID, parseErr := uuid.Parse(args.SubSessionID)
	if parseErr != nil {
		return fmt.Sprintf("Error: invalid sub_session_id format: %s", parseErr.Error()), nil
	}

	// 2. Determine current root session ID.
	rootSessionID := t.session.ID
	if t.session.RootServerSessionID != uuid.Nil {
		rootSessionID = t.session.RootServerSessionID
	}

	// 3. Query target session.
	targetSession, dbErr := t.helpers.deps.SessionRepo.GetByID(ctx, subSessionID)
	if dbErr != nil {
		return fmt.Sprintf("Error: sub agent session not found: %s", dbErr.Error()), nil
	}

	// 4. Verify target is a descendant of the same session tree.
	if targetSession.RootServerSessionID != rootSessionID {
		return "Error: target session is not a descendant of the current session tree", nil
	}
	if targetSession.ID == t.session.ID {
		return "Error: cannot query the current session itself", nil
	}

	// 5. Query recent messages and find the last one.
	recentMsgs, msgErr := t.helpers.deps.MessageRepo.ListRecentBySession(ctx, subSessionID, 50)
	if msgErr != nil {
		return fmt.Sprintf("Error: failed to list messages: %s", msgErr.Error()), nil
	}

	result := getSubAgentMessageResult{
		SubSessionID:     targetSession.ID.String(),
		SubSessionTitle:  targetSession.Title,
		SubSessionStatus: targetSession.Status,
	}

	if len(recentMsgs) > 0 {
		lastMsg := recentMsgs[len(recentMsgs)-1]

		// Resolve content (handles streaming messages by aggregating chunks from Redis).
		contentStr := t.helpers.deps.UpdatePublisher.ResolveMessageContent(lastMsg)
		var content protocol.ContentData
		if err := json.Unmarshal([]byte(contentStr), &content); err != nil {
			result.Content = "(failed to parse message content)"
		} else {
			result.Content = extractTextFromContent(content)
		}

		msgIDStr := lastMsg.ID.String()
		roleStr := lastMsg.Role
		createdAtStr := lastMsg.CreatedAt.Format(time.RFC3339)
		result.MessageID = &msgIDStr
		result.Role = &roleStr
		result.CreatedAt = &createdAtStr
	} else {
		result.Content = "No messages found in this session."
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("get_sub_agent_message: marshal result: %w", err)
	}
	resultStr := string(resultJSON)

	// 6. Get tool_call_id.
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("get_sub_agent_message: tool_call_id not set in context")
	}

	turnUUID := t.turnID
	if turnUUID == uuid.Nil {
		return "", fmt.Errorf("get_sub_agent_message: turn UUID is nil")
	}

	// 7. Create toolcall_input + toolcall_output messages and publish.
	toolCallData := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: "get_sub_agent_message",
		Input:    argumentsInJSON,
	}
	inputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: toolCallData,
	}

	completedStatus := "completed"
	outputToolCall := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: "get_sub_agent_message",
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
		return "", fmt.Errorf("get_sub_agent_message: publish messages: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "getSubAgentMessage.completed", map[string]any{
		"session_id":     t.session.ID.String(),
		"target_session": subSessionID.String(),
		"has_message":    result.MessageID != nil,
		"input_msg_id":   inputMsgID.String(),
		"output_msg_id":  outputMsgID.String(),
	})

	return resultStr, nil
}
