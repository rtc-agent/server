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
// The LLM provides an instruction (task description) and a mode (async or sync);
// the tool creates:
// 1. A sub session with parent/ root session hierarchy
// 2. A user message in the sub session (the instruction)
// 3. A sub_agent_invocation message in the parent session (for frontend rendering)
// 4. Submits a work item to the sub session's rtc-queue
//
// In **sync** mode (legacy default):
// 5. Interrupts the parent turn
// When the sub session completes, its final result is returned to the parent
// via the resume mechanism (WorkPayload.SubAgentResult).
//
// In **async** mode (default):
// 5. Returns immediately with the sub session ID
// When the sub session completes, a notification message is delivered to the
// parent session and a new turn is triggered via Submit.
type subAgentTool struct {
	session *model.Session
	helpers *helpers
	turnID  uuid.UUID
}

// subAgentArgs is the input schema for the sub_agent tool.
type subAgentArgs struct {
	Title       string `json:"title"`
	Instruction string `json:"instruction"`
	Mode        string `json:"mode"` // "async" (default) or "sync"
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
		Desc: subAgentDesc,
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
			"mode": {
				Type:     schema.String,
				Desc:     "Execution mode: \"async\" (default, parent continues immediately, result delivered as notification later) or \"sync\" (parent waits for result). Default is \"async\".",
				Required: false,
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
		return t.resumeSubAgent(ctx, state)
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

	// Normalize mode: default to "async".
	mode := args.Mode
	if mode == "" {
		mode = "async"
	}
	if mode != "sync" && mode != "async" {
		return fmt.Sprintf("Error: mode must be \"sync\" or \"async\", got %q", mode), nil
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

	// 4. Queue is required for submitting work to the sub session.
	if t.helpers.queue == nil {
		return "", fmt.Errorf("sub_agent: queue not available")
	}

	// 5. Generate sub session IDs.
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

	// 6. Create sub session + first message + parent message + publish updates.
	var parentMessageID uuid.UUID
	var subSessionMsgID uuid.UUID

	toolCallData := protocol.ToolCall{
		Id:       callID,
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
			SubAgentMode:          mode,
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
						EntityId: t.session.ID.String(),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: parentMessageID.String(),
					},
				},
			},
			{
				Channel: subChannel,
				Items: []protocol.UpdateItem{
					{
						Entity:   protocol.EntitySession,
						Action:   protocol.ActionUpdated,
						EntityId: subSessionID.String(),
					},
					{
						Entity:   protocol.EntityMessage,
						Action:   protocol.ActionCreated,
						EntityId: subSessionMsgID.String(),
					},
				},
			},
		}
		return items, nil
	})
	if err != nil {
		return "", fmt.Errorf("create sub agent: %w", err)
	}

	t.helpers.logger.Info(ctx, "subAgent.created", map[string]any{
		"sub_session_id":     subSessionID.String(),
		"parent_message_id":  parentMessageID.String(),
		"sub_first_msg_id":   subSessionMsgID.String(),
		"parent_session_id":  t.session.ID.String(),
		"turn_id":            turnUUID.String(),
		"instruction_length": len(args.Instruction),
	})

	// 7. Submit work item to sub session's rtc-queue.
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

	t.helpers.logger.Info(ctx, "subAgent.work_submitted", map[string]any{
		"sub_session_id": subSessionID.String(),
		"mode":           mode,
	})

	// 8. Async mode: create toolcall_output and return immediately.
	if mode == "async" {
		asyncResult := formatSubAgentAsyncResult(subSessionID.String(), args.Title)

		if err := publishOutputOnly(ctx, publishOutputOnlyInput{
			Helpers:         t.helpers,
			SessionID:       t.session.ID,
			OwnerRefID:      t.session.OwnerRefID,
			TurnID:          turnUUID,
			ToolName:        "sub_agent",
			ArgumentsInJSON: argumentsInJSON,
			Output:          asyncResult,
			ParentMessageID: parentMessageID,
		}); err != nil {
			return "", fmt.Errorf("publish async toolcall_output: %w", err)
		}

		t.helpers.logger.Info(ctx, "subAgent.async.returned_immediately", map[string]any{
			"sub_session_id":    subSessionID.String(),
			"parent_message_id": parentMessageID.String(),
		})

		return asyncResult, nil
	}

	// 9. Sync mode: build interrupt state and pause the turn.
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

	return "", tool.StatefulInterrupt(ctx, info, state)
}

// resumeSubAgent handles the resume path after a sub-agent interrupt.
// It checks whether the sub session has completed and, if so, retrieves the
// last assistant message as the result. If the sub session is still active,
// it re-interrupts to wait for completion.
func (t *subAgentTool) resumeSubAgent(ctx context.Context, state subAgentInterruptState) (string, error) {
	subSessionID, parseErr := uuid.Parse(state.SubSessionID)
	if parseErr != nil {
		return "", fmt.Errorf("sub_agent: invalid sub_session_id in state: %w", parseErr)
	}

	subSession, dbErr := t.helpers.deps.SessionRepo.GetByID(ctx, subSessionID)
	if dbErr != nil {
		t.helpers.logger.Warn(ctx, "subAgent.resume.db_error", map[string]any{
			"sub_session_id": subSessionID.String(),
			"error":          dbErr.Error(),
		})
		return t.reInterrupt(ctx, state)
	}

	// If sub session is still active or idle (not completed/failed/closed), re-interrupt.
	if subSession.Status != string(protocol.SessionStatusIdle) && subSession.Status != string(protocol.SessionStatusClosed) {
		return t.reInterrupt(ctx, state)
	}

	// Sub session completed. Retrieve the last assistant message.
	resultText, err := t.fetchLastAssistantText(ctx, subSessionID)
	if err != nil {
		return formatSubAgentNoResult(), nil
	}

	t.helpers.logger.Info(ctx, "subAgent.resume.completed", map[string]any{
		"sub_session_id":    subSessionID.String(),
		"result_length":     len(resultText),
		"parent_message_id": state.ParentMessageID,
	})

	return resultText, nil
}

// reInterrupt issues a StatefulInterrupt to keep waiting for the sub session.
func (t *subAgentTool) reInterrupt(ctx context.Context, state subAgentInterruptState) (string, error) {
	info := subAgentInterruptInfo{
		Type:            "sub_agent",
		SubSessionID:    state.SubSessionID,
		ParentMessageID: state.ParentMessageID,
	}
	return "", tool.StatefulInterrupt(ctx, info, state)
}

// fetchLastAssistantText retrieves the text content of the most recent
// assistant message in the given session. Returns a user-friendly fallback
// string if no message is found or content cannot be parsed.
func (t *subAgentTool) fetchLastAssistantText(ctx context.Context, sessionID uuid.UUID) (string, error) {
	recentMsgs, err := t.helpers.deps.MessageRepo.ListRecentBySession(ctx, sessionID, 50)
	if err != nil {
		t.helpers.logger.Warn(ctx, "subAgent.resume.get_last_message_failed", map[string]any{
			"sub_session_id": sessionID.String(),
			"error":          err.Error(),
		})
		return "", err
	}

	var lastMsg *model.Message
	for i := len(recentMsgs) - 1; i >= 0; i-- {
		if recentMsgs[i].Role == string(protocol.MessageRoleAssistant) {
			lastMsg = recentMsgs[i]
			break
		}
	}
	if lastMsg == nil {
		return formatSubAgentNoAssistant(), nil
	}

	var content protocol.ContentData
	if err := json.Unmarshal([]byte(lastMsg.Content), &content); err != nil {
		t.helpers.logger.Warn(ctx, "subAgent.resume.deserialize_content_failed", map[string]any{
			"sub_session_id": sessionID.String(),
			"error":          err.Error(),
		})
		return formatSubAgentParseFailed(), nil
	}

	resultText := extractTextFromContent(content)
	if resultText == "" {
		resultText = formatSubAgentNoOutput()
	}
	return resultText, nil
}
