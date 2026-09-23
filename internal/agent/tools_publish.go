package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/channel"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// publishToolMessages — shared helper for toolcall_input + toolcall_output
// ---------------------------------------------------------------------------

// publishToolMessagesInput holds all parameters needed to create and publish
// the standard toolcall_input + toolcall_output message pair.
type publishToolMessagesInput struct {
	Helpers         *helpers
	SessionID       uuid.UUID
	OwnerRefID      string
	TurnID          uuid.UUID
	ToolName        string
	ArgumentsInJSON string
	ResultJSON      string // pre-marshaled JSON string (caller calls mustMarshalJSON once)
}

// publishToolMessages creates the toolcall_input + toolcall_output messages
// for a tool invocation and publishes them (with EntityMessage.created events)
// in a single transaction.
//
// This is the standard pattern used by goal, loop, and sub-agent query tools.
func publishToolMessages(ctx context.Context, in publishToolMessagesInput) error {
	resultJSON := in.ResultJSON
	completedStatus := "completed"

	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return fmt.Errorf("%s: tool_call_id not set in context", in.ToolName)
	}
	if in.TurnID == uuid.Nil {
		return fmt.Errorf("%s: turn UUID is nil", in.ToolName)
	}

	// 规范化 ArgumentsInJSON：空字符串或 "null" 统一为 "{}"
	// 这确保 DB 存储和 eino 内存中的表示一致，避免缓存失效
	argumentsInJSON := normalizeToolArguments(in.ArgumentsInJSON)

	inputToolCall := protocol.ToolCall{
		Id:       callID,
		ToolName: in.ToolName,
		Input:    argumentsInJSON,
	}
	inputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: inputToolCall,
	}

	outputToolCall := protocol.ToolCall{
		Id:       callID,
		ToolName: in.ToolName,
		Input:    argumentsInJSON,
		Output:   &resultJSON,
		Status:   &completedStatus,
	}
	outputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: outputToolCall,
	}

	_, err := in.Helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		inputMsg, createErr := primitives.CreateMessage(
			txCtx, in.Helpers.deps,
			in.SessionID, &in.TurnID,
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
		inputMsgID := inputMsg.ID

		outputMsg, createErr := primitives.CreateMessage(
			txCtx, in.Helpers.deps,
			in.SessionID, &in.TurnID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			outputContent,
			protocol.MessageStreamingCompleted,
			"",          // system-generated
			&inputMsgID, // parent = toolcall_input
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_output: %w", createErr)
		}

		ch := channel.UserTopic(in.OwnerRefID)
		updateItems := []updates.UpdatePublishItem{
			{
				Channel: ch,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: inputMsgID.String(),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: outputMsg.ID.String(),
					},
				},
			},
		}
		return updateItems, nil
	})
	return err
}

// ---------------------------------------------------------------------------
// publishOutputOnly — async branch: only toolcall_output (parent already exists)
// ---------------------------------------------------------------------------

// publishOutputOnlyInput holds parameters for creating a toolcall_output message
// when the parent toolcall_input already exists (async subAgent pattern).
type publishOutputOnlyInput struct {
	Helpers         *helpers
	SessionID       uuid.UUID
	OwnerRefID      string
	TurnID          uuid.UUID
	ToolName        string
	ArgumentsInJSON string
	Output          string
	ParentMessageID uuid.UUID
}

// publishOutputOnly creates only a toolcall_output message (parent is an
// existing toolcall_input) and publishes EntityMessage.created event.
// Used by the async branch of subAgent tool.
func publishOutputOnly(ctx context.Context, in publishOutputOnlyInput) error {
	completedStatus := "completed"

	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return fmt.Errorf("%s: tool_call_id not set in context", in.ToolName)
	}

	// 规范化 ArgumentsInJSON：空字符串或 "null" 统一为 "{}"
	argumentsInJSON := normalizeToolArguments(in.ArgumentsInJSON)

	outputToolCall := protocol.ToolCall{
		Id:       callID,
		ToolName: in.ToolName,
		Input:    argumentsInJSON,
		Output:   &in.Output,
		Status:   &completedStatus,
	}
	outputContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: outputToolCall,
	}

	_, err := in.Helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		outputMsg, createErr := primitives.CreateMessage(
			txCtx, in.Helpers.deps,
			in.SessionID, &in.TurnID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			outputContent,
			protocol.MessageStreamingCompleted,
			"",
			&in.ParentMessageID,
		)
		if createErr != nil {
			return nil, fmt.Errorf("create async toolcall_output: %w", createErr)
		}

		ch := channel.UserTopic(in.OwnerRefID)
		return []updates.UpdatePublishItem{
			{
				Channel: ch,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: outputMsg.ID.String(),
					},
				},
			},
		}, nil
	})
	return err
}

// ---------------------------------------------------------------------------
// mustMarshalJSON — JSON marshalling with error propagation
// ---------------------------------------------------------------------------

// mustMarshalJSON marshals v to JSON, returning an error instead of panicking.
func mustMarshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}
	return string(b), nil
}

// ---------------------------------------------------------------------------
// normalizeToolArguments — normalize empty/null arguments to "{}"
// ---------------------------------------------------------------------------

// normalizeToolArguments normalizes empty or null JSON arguments to "{}".
// This ensures consistency between DB storage and eino's in-memory representation,
// preventing cache invalidation caused by "{}" vs null differences.
//
// When the LLM calls a tool with no arguments, different code paths may produce:
//   - Empty string ""
//   - Literal "null"
//   - Empty object "{}"
//
// Eino's in-memory representation normalizes to "{}", so we do the same here.
func normalizeToolArguments(argumentsInJSON string) string {
	trimmed := strings.TrimSpace(argumentsInJSON)
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	return argumentsInJSON
}
