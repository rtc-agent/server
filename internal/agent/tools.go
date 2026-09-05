package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// Each tool's Info returns the tool metadata; InvokableRun delegates to
// rtcToolBase.InvokableRun which implements the full RTC tool logic
// (create Message + RTC record, publish updates, stateful interrupt).

// --- lsTool ---

type lsTool struct{ base *rtcToolBase }

func (l *lsTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "ls",
		Desc: "List directory contents at the given path",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path": {Type: schema.String, Desc: "The directory path to list (defaults to current directory)", Required: false},
		}),
	}, nil
}

func (l *lsTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return l.base.InvokableRun(ctx, "ls", argumentsInJSON, opts...)
}

// --- readTool ---

type readTool struct{ base *rtcToolBase }

func (t *readTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "read",
		Desc: "Read file contents from the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path":   {Type: schema.String, Desc: "The file path to read", Required: true},
			"offset": {Type: schema.Integer, Desc: "Byte offset to start reading from (default: 0)", Required: false},
			"limit":  {Type: schema.Integer, Desc: "Maximum number of bytes to read (default: unlimited)", Required: false},
		}),
	}, nil
}

func (t *readTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "read", argumentsInJSON, opts...)
}

// --- writeTool ---

type writeTool struct{ base *rtcToolBase }

func (t *writeTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "write",
		Desc: "Write or create a file in the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path":    {Type: schema.String, Desc: "The file path to write to", Required: true},
			"content": {Type: schema.String, Desc: "The content to write", Required: true},
			"mode":    {Type: schema.String, Desc: "Write mode: 'overwrite' (default) or 'append'", Required: false},
		}),
	}, nil
}

func (t *writeTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "write", argumentsInJSON, opts...)
}

// --- grepTool ---

type grepTool struct{ base *rtcToolBase }

func (t *grepTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "grep",
		Desc: "Search for a pattern across file contents in the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"pattern":        {Type: schema.String, Desc: "Regex pattern to search for", Required: true},
			"path":           {Type: schema.String, Desc: "File or directory path to search in (default: root '/')", Required: false},
			"case_sensitive": {Type: schema.Boolean, Desc: "Whether the search is case-sensitive (default: false)", Required: false},
		}),
	}, nil
}

func (t *grepTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "grep", argumentsInJSON, opts...)
}

// --- findTool ---

type findTool struct{ base *rtcToolBase }

func (t *findTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "find",
		Desc: "Find files by name pattern in the virtual filesystem",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"pattern": {Type: schema.String, Desc: "Glob pattern to match file names (e.g. '*.ts', '**/*.go')", Required: true},
			"path":    {Type: schema.String, Desc: "Directory path to search in (default: root '/')", Required: false},
		}),
	}, nil
}

func (t *findTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "find", argumentsInJSON, opts...)
}

// --- scriptTool ---

type scriptTool struct{ base *rtcToolBase }

func (t *scriptTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "script",
		Desc: "Execute JavaScript code in the browser environment. Provide either a file path or inline code (mutually exclusive)",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"action": {
				Type:     schema.String,
				Enum:     []string{"run", "save", "eval"},
				Desc:     "The action to perform: 'save' persists the inline code to /scripts/{name}.ts (requires both 'code' and 'name'); 'run' executes a previously saved script by name (requires 'name'); 'eval' executes the inline code directly (requires 'code'). Defaults to 'eval' when omitted.",
				Required: false,
			},
			"name": {Type: schema.String, Desc: "The script name. Required for 'save' and 'run'.", Required: false},
			"code": {Type: schema.String, Desc: "Inline JavaScript code to execute", Required: false},
		}),
	}, nil
}

func (t *scriptTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.base.InvokableRun(ctx, "script", argumentsInJSON, opts...)
}

// InvokableRun implements the full RTC tool logic for all tool types.
//
// First-call path: creates a Message (type=toolcall_input) + RTC record in DB,
// publishes message.created and rtc.updated events, then calls
// tool.StatefulInterrupt to pause the turn and wait for client execution.
//
// Resume path: called again after checkpoint restore. Checks if the RTC record
// has reached a terminal state (completed/failed/timeout/rejected). If so,
// returns the formatted result. If not, re-interrupts with the same RTC ID.
//
// Mapping from old code: this is the direct equivalent of BaseRTC.InvokableRun
// in internal/worker/tools.rtc.go. Key differences:
//   - turnID is stored on rtcToolBase (set in createTools) instead of read
//     from context via getTurnUUID(ctx). The old code threaded turnID through
//     context because the tool was created once per session; the new code
//     creates tools per-turn so turnID is known at construction time.
//   - r.manager.deps -> r.helpers.deps (the integration struct is helpers, not Manager)
//   - Logger calls use h.logIfEnabled instead of logger.Debug/Info directly.
func (r *rtcToolBase) InvokableRun(ctx context.Context, toolName string, argumentsInJSON string, _ ...tool.Option) (string, error) {
	// === Resume path ===
	wasInterrupted, hasState, state := tool.GetInterruptState[rtcInterruptState](ctx)
	if wasInterrupted {
		if !hasState {
			return "", fmt.Errorf("rtc: state type mismatch on resume")
		}

		// Checkpoint recovery: check if the RTC already has a result in DB
		// (the client may have submitted it while the worker was down).
		rtcID, parseErr := uuid.Parse(state.RtcID)
		if parseErr != nil {
			return "", fmt.Errorf("rtc: invalid rtc_id in state: %w", parseErr)
		}
		dbRtc, dbErr := r.helpers.deps.RtcRepo.GetByID(ctx, rtcID)
		if dbErr != nil {
			// DB query failed: degrade to re-interrupt and wait for client to
			// re-submit. This matches the old code's fallback behavior.
			r.helpers.logIfEnabled(ctx, "rtcToolBase.resume.db_error", map[string]any{
				"rtc_id": rtcID.String(),
				"error":  dbErr.Error(),
			})
			info := rtcInterruptInfo{
				Type:      "rtc",
				ToolName:  state.ToolName,
				RtcID:     state.RtcID,
				MessageID: state.MessageID,
			}
			return "", tool.StatefulInterrupt(ctx, info, state)
		}

		// RTC reached terminal state -> return formatted result, no more interrupt.
		switch protocol.RtcStatus(dbRtc.Status) {
		case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
			protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
			toolOutput := string(dbRtc.Result)
			if dbRtc.Status == string(protocol.RtcStatusFailed) && dbRtc.ErrorMessage != "" {
				toolOutput = dbRtc.ErrorMessage
			}
			tc := protocol.ToolCall{
				Id:       protocol.UUID(state.ToolCallID),
				ToolName: dbRtc.ToolName,
				Output:   &toolOutput,
				Status:   &dbRtc.Status,
			}
			return formatToolCallOutput(tc), nil
		}

		// RTC not yet terminal (pending/sent/executing) -> re-interrupt with
		// same RTC ID so the tool waits again.
		info := rtcInterruptInfo{
			Type:      "rtc",
			ToolName:  state.ToolName,
			RtcID:     state.RtcID,
			MessageID: state.MessageID,
		}
		return "", tool.StatefulInterrupt(ctx, info, state)
	}

	// === First-call path ===
	// 1. Get tool_call_id (eino injects it into context before calling the tool).
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", fmt.Errorf("rtc: tool_call_id not set in context")
	}

	// 2. Turn ID is already known (stored on rtcToolBase at construction time).
	// In the old code, this was read from context via getTurnUUID(ctx).
	turnUUID := r.turnID
	if turnUUID == uuid.Nil {
		return "", fmt.Errorf("rtc: turn UUID is nil")
	}

	// 3. Generate RTC ID and ClientID.
	rtcID := uuid.Must(uuid.NewV7())
	clientID := uuid.Must(uuid.NewV7()).String()

	// 4. Build toolcall_input ContentData.
	toolCallData := protocol.ToolCall{
		Id:       protocol.UUID(callID),
		ToolName: toolName,
		Input:    argumentsInJSON,
	}
	contentData := protocol.ContentData{
		Type: protocol.ContentTypeToolCallInput,
		Data: toolCallData,
	}

	// 5. Transactionally create Message + RTC record + publish updates.
	var msgID uuid.UUID
	_, err := r.helpers.deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		// Allocate RTC offset (reuses the session:msg_offset counter).
		rtcOffset, _, offsetErr := primitives.AllocateOffsets(txCtx, r.helpers.deps, r.session.ID, &turnUUID, 1)
		if offsetErr != nil {
			return nil, fmt.Errorf("allocate rtc offset: %w", offsetErr)
		}

		// Create Message (role=tool, type=toolcall_input).
		msg, createErr := primitives.CreateMessage(
			txCtx, r.helpers.deps,
			r.session.ID, &turnUUID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{},
			contentData,
			protocol.MessageStreamingCompleted,
			"",  // system-generated
			nil, // no parent message
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall message: %w", createErr)
		}
		msgID = msg.ID

		// Create RTC record (MessageID points to the toolcall_input message).
		rtc := &model.Rtc{
			ID:         rtcID,
			ClientID:   clientID,
			SessionID:  r.session.ID,
			TurnID:     turnUUID,
			MessageID:  msgID,
			Offset:     rtcOffset,
			ToolName:   toolName,
			Parameters: model.JSONBString(argumentsInJSON),
			Status:     string(model.RtcStatusPending),
		}
		if err := r.helpers.deps.RtcRepo.Create(txCtx, rtc); err != nil {
			return nil, fmt.Errorf("create rtc: %w", err)
		}

		// Build publish updates (RTC status + toolcall_input message created).
		items := primitives.BuildRtcStatusUpdates(r.session, rtcID)
		if len(items) > 0 {
			items[0].Items = append(items[0].Items, protocol.UpdateItem{
				Entity:   protocol.EntityMessage,
				Action:   protocol.ActionCreated,
				EntityId: protocol.UUID(msgID.String()),
			})
		}
		return items, nil
	})
	if err != nil {
		return "", fmt.Errorf("create rtc and message: %w", err)
	}

	r.helpers.logIfEnabled(ctx, "rtcToolBase.rtc_created", map[string]any{
		"tool_name":  toolName,
		"rtc_id":     rtcID.String(),
		"message_id": msgID.String(),
		"turn_id":    turnUUID.String(),
	})

	// 6. Build interrupt state.
	state = rtcInterruptState{
		RtcID:      rtcID.String(),
		ToolCallID: callID,
		MessageID:  msgID.String(),
		ToolName:   toolName,
	}

	// 7. Build interrupt info.
	info := rtcInterruptInfo{
		Type:      "rtc",
		ToolName:  toolName,
		RtcID:     rtcID.String(),
		MessageID: msgID.String(),
		Args:      argumentsInJSON,
	}

	// 8. Pause the turn — eino will persist the checkpoint and return control
	// to turn-agent, which calls InterruptTurn.
	return "", tool.StatefulInterrupt(ctx, info, state)
}
