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
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// subAgentTool enables the LLM to create sub agent sessions for task decomposition.
//
// The LLM provides an instruction (task description); the tool creates:
// 1. A sub session with parent/ root session hierarchy
// 2. A user message in the sub session (the instruction)
// 3. A sub_agent_invocation message in the parent session (for frontend rendering)
// 4. Submits a work item to the sub session's rtc-queue
// 5. Interrupts the parent turn
//
// When the sub session completes, its final result is returned to the parent
// via the resume mechanism (WorkPayload.SubAgentResult).
type subAgentTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// subAgentArgs is the input schema for the sub_agent tool.
type subAgentArgs struct {
	Title       string `json:"title"`
	Instruction string `json:"instruction"`
}

// subAgentInterruptInfo is passed to StatefulInterrupt as the info parameter.
// Must be gob-serializable.
type subAgentInterruptInfo struct {
	Type            string // "sub_agent"
	SubSessionID    string
	ParentMessageID string
	Instruction     string
}

// subAgentInterruptState is passed to StatefulInterrupt as the state parameter.
// When the tool resumes from a checkpoint, it reads this state to find the result.
// Must be gob-serializable.
type subAgentInterruptState struct {
	SubSessionID    string
	ToolCallID      string
	ParentMessageID string
}

func (t *subAgentTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "sub_agent",
		Desc: `Create a sub agent session to handle a complex, multi-step task.

The sub agent runs in its own session with a fresh context. The parent session is paused until the sub agent completes, then its final response is returned as the tool result.

Usage notes:
- Always include a short title (3-5 words) summarizing the task
- The sub agent starts with a blank context. Brief the agent like a smart colleague who just walked into the room — it hasn't seen this conversation, doesn't know what you've tried, doesn't understand why this task matters
- Explain what you're trying to accomplish and why. Describe what you've already learned or ruled out
- Never delegate understanding. Don't write vague instructions like "handle this task" — include specific requirements, file paths, and relevant context`,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"title": {
				Type:     schema.String,
				Desc:     "A short (3-5 word) noun-phrase description of the task. Examples: 'OAuth authentication setup', 'Login button fix', '用户登录功能实现'",
				Required: true,
			},
			"instruction": {
				Type:     schema.String,
				Desc:     "The task instruction for the sub agent. Be specific and include all necessary context. The sub agent starts with a blank context, so include relevant details. Example: 'Verify if goal X is completed by checking the TodoList and recent messages'",
				Required: true,
			},
		}),
	}, nil
}

func (t *subAgentTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// === Resume path ===
	wasInterrupted, hasState, state := tool.GetInterruptState[subAgentInterruptState](ctx)
	if wasInterrupted {
		if !hasState {
			return "", fmt.Errorf("sub_agent: state type mismatch on resume")
		}

		// Check if the sub session has completed.
		subSessionID, parseErr := uuid.Parse(state.SubSessionID)
		if parseErr != nil {
			return "", fmt.Errorf("sub_agent: invalid sub_session_id in state: %w", parseErr)
		}

		subSession, dbErr := t.helpers.deps.SessionRepo.GetByID(ctx, subSessionID)
		if dbErr != nil {
			t.helpers.logIfEnabled(ctx, "subAgent.resume.db_error", map[string]any{
				"sub_session_id": subSessionID.String(),
				"error":          dbErr.Error(),
			})
			// Re-interrupt and wait for sub session to complete.
			info := subAgentInterruptInfo{
				Type:            "sub_agent",
				SubSessionID:    state.SubSessionID,
				ParentMessageID: state.ParentMessageID,
			}
			return "", tool.StatefulInterrupt(ctx, info, state)
		}

		// If sub session is still active or idle (not completed/failed/closed), re-interrupt.
		if subSession.Status != string(protocol.SessionStatusIdle) && subSession.Status != string(protocol.SessionStatusClosed) {
			info := subAgentInterruptInfo{
				Type:            "sub_agent",
				SubSessionID:    state.SubSessionID,
				ParentMessageID: state.ParentMessageID,
			}
			return "", tool.StatefulInterrupt(ctx, info, state)
		}

		// Sub session completed. Get the last assistant message as the result.
		// The result is already passed via WorkPayload.SubAgentResult when the
		// parent session was resumed, so we need to retrieve it from the checkpoint
		// context or the last message in the sub session.
		//
		// For now, we'll fetch the last assistant message from the sub session.
		// This is a simplification; the ideal approach would be to pass the result
		// through the interrupt state, but that requires changes to the checkpoint
		// mechanism.
		recentMsgs, msgErr := t.helpers.deps.MessageRepo.ListRecentBySession(ctx, subSessionID, 50)
		if msgErr != nil {
			t.helpers.logIfEnabled(ctx, "subAgent.resume.get_last_message_failed", map[string]any{
				"sub_session_id": subSessionID.String(),
				"error":          msgErr.Error(),
			})
			return "Sub agent completed, but no result message found.", nil
		}

		// Find the last assistant message.
		var lastMsg *model.Message
		for i := len(recentMsgs) - 1; i >= 0; i-- {
			if recentMsgs[i].Role == string(protocol.MessageRoleAssistant) {
				lastMsg = recentMsgs[i]
				break
			}
		}
		if lastMsg == nil {
			return "Sub agent completed, but no assistant message found.", nil
		}

		// Deserialize the message content.
		var content protocol.ContentData
		if err := json.Unmarshal([]byte(lastMsg.Content), &content); err != nil {
			t.helpers.logIfEnabled(ctx, "subAgent.resume.deserialize_content_failed", map[string]any{
				"sub_session_id": subSessionID.String(),
				"error":          err.Error(),
			})
			return "Sub agent completed, but result could not be parsed.", nil
		}

		// Extract the text content.
		resultText := extractTextFromContent(content)

		if resultText == "" {
			resultText = "Sub agent completed with no output."
		}

		t.helpers.logIfEnabled(ctx, "subAgent.resume.completed", map[string]any{
			"sub_session_id":    subSessionID.String(),
			"result_length":     len(resultText),
			"parent_message_id": state.ParentMessageID,
		})

		return resultText, nil
	}

	// === First-call path ===
	// 1. Parse arguments.
	var args subAgentArgs
	if ok, errMsg := parseToolArgs(ctx, t.helpers, "sub_agent", argumentsInJSON, &args); !ok {
		return errMsg, nil
	}

	if args.Title == "" {
		return "Error: title is required", nil
	}

	if args.Instruction == "" {
		return "Error: instruction is required", nil
	}

	// 2. Get tool_call_id (eino injects it into context before calling the tool).
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("sub_agent: tool_call_id not set in context")
	}

	// 3. Turn ID is already known (stored on subAgentTool at construction time).
	turnUUID := t.turnID
	if turnUUID == uuid.Nil {
		return "", fmt.Errorf("sub_agent: turn UUID is nil")
	}

	// 4. Generate sub session IDs.
	subSessionID := uuid.Must(uuid.NewV7())
	subSessionClientID := uuid.Must(uuid.NewV7()).String()

	// Determine root session: if the current session is already a sub session,
	// use its root; otherwise, the current session is the root.
	rootClientSessionID := t.session.ClientID
	rootServerSessionID := t.session.ID
	if t.session.RootServerSessionID != uuid.Nil {
		rootClientSessionID = t.session.RootClientSessionID
		rootServerSessionID = t.session.RootServerSessionID
	}

	// 5. Create sub session + first message + parent message + publish updates.
	var parentMessageID uuid.UUID
	var subSessionMsgID uuid.UUID

	toolCallData := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: "sub_agent",
		Input:    argumentsInJSON,
	}
	parentContent := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: toolCallData,
	}

	_, err := t.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		// Create sub session.
		subSession := &model.Session{
			ID:                    subSessionID,
			ClientID:              subSessionClientID,
			OwnerKind:             t.session.OwnerKind,
			OwnerRefID:            t.session.OwnerRefID,
			DeviceID:              t.session.DeviceID,
			Title:                 args.Title,
			Status:                string(protocol.SessionStatusActive),
			AgentPrompt:           t.session.AgentPrompt,
			ParentClientSessionID: t.session.ClientID,
			ParentServerSessionID: t.session.ID,
			RootClientSessionID:   rootClientSessionID,
			RootServerSessionID:   rootServerSessionID,
			CreatedAt:             time.Now(),
			UpdatedAt:             time.Now(),
			ClosedAt:              nil,
			DeletedAt:             nil,
		}
		if err := t.helpers.deps.SessionRepo.Create(txCtx, subSession); err != nil {
			return nil, fmt.Errorf("create sub session: %w", err)
		}

		// Create first message in sub session (role=user, the instruction).
		subContent := protocol.ContentData{
			Type: protocol.ContentTypeText,
			Data: args.Instruction,
		}
		subMsg, err := primitives.CreateMessage(
			txCtx, t.helpers.deps,
			subSessionID, nil, // no turn ID for the first message
			protocol.MessageRoleUser,
			usecase.SystemCreator{}, // system-created (agent伪装)
			subContent,
			protocol.MessageStreamingCompleted,
			"",  // auto-generate client ID
			nil, // no parent message
		)
		if err != nil {
			return nil, fmt.Errorf("create sub session first message: %w", err)
		}
		subSessionMsgID = subMsg.ID

		parentMsg, err := primitives.CreateMessage(
			txCtx, t.helpers.deps,
			t.session.ID, &turnUUID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			parentContent,
			protocol.MessageStreamingCompleted,
			"",  // auto-generate client ID
			nil, // no parent message
		)
		if err != nil {
			return nil, fmt.Errorf("create parent session sub_agent_invocation message: %w", err)
		}
		parentMessageID = parentMsg.ID

		// Update sub session with parent message ID.
		if err := t.helpers.deps.SessionRepo.UpdateFieldsActive(txCtx, subSessionID, map[string]any{
			"sub_agent_parent_message_id": parentMessageID,
		}); err != nil {
			return nil, fmt.Errorf("update sub session parent message ID: %w", err)
		}

		// Build publish updates.
		// Use channel.UserTopic to construct the proper channel format.
		parentChannel := channel.UserTopic(t.session.OwnerRefID)
		subChannel := channel.UserTopic(subSession.OwnerRefID)

		items := []updates.UpdatePublishItem{
			{
				Channel: parentChannel,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntitySession,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(t.session.ID.String()),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(parentMessageID.String()),
					},
				},
			},
			{
				Channel: subChannel,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntitySession,
						Action:   protocol.ActionUpdated,
						EntityId: protocol.UUID(subSessionID.String()),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: protocol.UUID(subSessionMsgID.String()),
					},
				},
			},
		}
		return items, nil
	})
	if err != nil {
		return "", fmt.Errorf("create sub agent: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "subAgent.created", map[string]any{
		"sub_session_id":     subSessionID.String(),
		"parent_message_id":  parentMessageID.String(),
		"sub_first_msg_id":   subSessionMsgID.String(),
		"parent_session_id":  t.session.ID.String(),
		"turn_id":            turnUUID.String(),
		"instruction_length": len(args.Instruction),
	})

	// 6. Submit work item to sub session's rtc-queue.
	if t.helpers.queue == nil {
		return "", fmt.Errorf("sub_agent: queue not available")
	}

	payload, err := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: subSessionID.String(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal work payload: %w", err)
	}

	if _, err := t.helpers.queue.Publish(ctx, subSessionID.String(), string(payload), 0); err != nil {
		return "", fmt.Errorf("publish work item to sub session: %w", err)
	}

	t.helpers.logIfEnabled(ctx, "subAgent.work_submitted", map[string]any{
		"sub_session_id": subSessionID.String(),
	})

	// 7. Build interrupt state and info.
	state = subAgentInterruptState{
		SubSessionID:    subSessionID.String(),
		ToolCallID:      callID,
		ParentMessageID: parentMessageID.String(),
	}

	info := subAgentInterruptInfo{
		Type:            "sub_agent",
		SubSessionID:    subSessionID.String(),
		ParentMessageID: parentMessageID.String(),
		Instruction:     args.Instruction,
	}

	// 8. Pause the turn.
	return "", tool.StatefulInterrupt(ctx, info, state)
}
