package agent

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func init() {
	// eino's checkpoint uses encoding/gob to serialize interrupt state.
	// Concrete types stored in interface{} must be registered via gob.Register,
	// otherwise serialization fails.
	//
	// rtcInterruptState: StatefulInterrupt's state parameter -> InterruptState.State
	// rtcInterruptInfo:  StatefulInterrupt's info parameter  -> InterruptCtx.Info
	//
	// Mapping from old code: identical to the gob.Register calls in
	// internal/worker/tools.rtc.go's init().
	gob.Register(rtcInterruptState{})
	gob.Register(rtcInterruptInfo{})
}

// =============================================================================
// Data callbacks
// =============================================================================
//
// These four callbacks handle the data flow between the agent and the
// application. They use turn-agent's own types (Message, Event) — the
// application never writes code that consumes eino's flow primitives.

// loadMessages loads the conversation history for a session from the DB and
// converts it to turn-agent Message types.
//
// Called by eino's GenInput path (fresh turns only — eino does not call
// GenInput when resuming from a checkpoint).
//
// Mapping from old code: this replaces loadMessageHistory in
// internal/worker/context.go. The key difference is the return type:
// []*turnagent.Message instead of []*schema.Message. The conversion from
// model.Message to turnagent.Message mirrors the old conversion to
// schema.Message, but uses the pkg-level types.
func (h *helpers) loadMessages(ctx context.Context, sessionID string) ([]*turnagent.Message, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: invalid session ID %q: %w", sessionID, err)
	}

	const historyLimit = 200
	dbMsgs, err := h.deps.MessageRepo.ListRecentBySession(ctx, sid, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("loadMessages: list messages for session %s: %w", sessionID, err)
	}

	// Find the most recent summary message (if any) and truncate the history
	// to start from it. Messages before the summary are already compressed
	// into it and should not be sent to the agent.
	//
	// ListRecentBySession returns messages in ASC order (oldest first within
	// the returned set), so we iterate backwards to find the first summary.
	tmpMsgs := make([]*model.Message, 0, len(dbMsgs))
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		msg := dbMsgs[i]
		tmpMsgs = append(tmpMsgs, msg)
		contentData, parseErr := primitives.ParseContentData(msg.Content)
		if parseErr == nil && contentData.Type == protocol.ContentTypeSummary {
			break
		}
	}
	// Reverse to get chronological order (oldest first).
	sort.Slice(tmpMsgs, func(i, j int) bool {
		return tmpMsgs[i].GlobalOffset < tmpMsgs[j].GlobalOffset
	})
	dbMsgs = tmpMsgs

	// Convert DB messages to turn-agent Messages.
	// The conversion logic mirrors the old SchemaMessages method in context.go,
	// but produces turnagent.Message instead of schema.Message.
	var messages []*turnagent.Message
	for _, msg := range dbMsgs {
		converted := convertDBMessage(msg)
		messages = append(messages, converted...)
	}

	return messages, nil
}

// convertDBMessage converts a single model.Message to zero or more
// turnagent.Messages.
//
// A single DB message may produce multiple turnagent messages (e.g., a
// toolcall_input produces an assistant message with tool calls; a summary
// may expand into multiple messages).
func convertDBMessage(msg *model.Message) []*turnagent.Message {
	contentData, err := primitives.ParseContentData(msg.Content)
	if err != nil {
		return nil
	}

	switch contentData.Type {
	case protocol.ContentTypeSummary:
		return convertSummaryContent(contentData.Data)

	case protocol.ContentTypeText, protocol.ContentTypeMarkdown:
		text, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:    msg.Role,
			Content: text,
		}}

	case protocol.ContentTypeThinking:
		tk, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:             msg.Role,
			ReasoningContent: tk,
		}}

	case protocol.ContentTypeToolCallInput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil
		}
		// Produce an assistant message with tool calls.
		return []*turnagent.Message{{
			Role: string(schema.Assistant),
			ToolCalls: []turnagent.ToolCall{{
				ID:        string(toolCall.Id),
				Name:      toolCall.ToolName,
				Arguments: toolCall.Input,
			}},
		}}

	case protocol.ContentTypeToolCallOutput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil
		}
		content := formatToolCallOutput(toolCall)
		return []*turnagent.Message{{
			Role:       string(schema.Tool),
			Content:    content,
			ToolName:   toolCall.ToolName,
			ToolCallID: string(toolCall.Id),
		}}

	default:
		return nil
	}
}

// convertSummaryContent expands a summary content block into multiple messages.
func convertSummaryContent(data any) []*turnagent.Message {
	dataBytes, err := primitives.ContentDataBytes(data)
	if err != nil {
		return nil
	}
	var items []primitives.SummaryItem
	if err := json.Unmarshal(dataBytes, &items); err != nil {
		return nil
	}
	var msgs []*turnagent.Message
	for _, item := range items {
		msgs = append(msgs, &turnagent.Message{
			Role:    item.Role,
			Content: item.Content,
		})
	}
	return msgs
}

// formatToolCallOutput formats a tool call's output for injection into the
// agent's context. Mirrors the old formatToolCallOutput in context.go.
func formatToolCallOutput(toolCall protocol.ToolCall) string {
	output := ""
	if toolCall.Output != nil {
		output = *toolCall.Output
	}
	status := ""
	if toolCall.Status != nil {
		status = *toolCall.Status
	}

	switch protocol.RtcStatus(status) {
	case protocol.RtcStatusCompleted:
		return output
	case protocol.RtcStatusFailed:
		return fmt.Sprintf("[Tool Error] %s failed: %s", toolCall.ToolName, output)
	case protocol.RtcStatusTimeout:
		return fmt.Sprintf("[Tool Timeout] %s timed out", toolCall.ToolName)
	case protocol.RtcStatusRejected:
		return fmt.Sprintf("[Tool Rejected] %s was rejected by user", toolCall.ToolName)
	default:
		return fmt.Sprintf("[Tool Pending] %s is still %s", toolCall.ToolName, status)
	}
}

// createTools creates the tool list for a session's turn.
//
// turnID is provided so the implementation can inject it into the tool's
// context (e.g., so tool.InvokableRun can read it via context.Value).
//
// Mapping from old code: this replaces createTools in
// internal/worker/tools.rtc.go. The key difference is the callback signature:
// it takes turnID as a parameter (so the tool can be turn-aware) instead of
// getting it from a session-level context value.
//
// The tools (lsTool, readTool, etc.) are defined in this package with their
// own rtcToolBase type, adapted to the new turnID-passing pattern.
func (h *helpers) createTools(ctx context.Context, sessionID string, turnID string) ([]tool.BaseTool, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, fmt.Errorf("createTools: invalid session ID %q: %w", sessionID, err)
	}
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return nil, fmt.Errorf("createTools: invalid turn ID %q: %w", turnID, err)
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("createTools: get session %s: %w", sessionID, err)
	}

	// Create RTC tool base with session and turn awareness.
	// The BaseRTC needs access to the helpers (for DB operations) and the
	// current turn ID (for creating RTC records tied to the correct turn).
	base := &rtcToolBase{
		session: session,
		helpers: h,
		turnID:  tid,
	}

	tools := []tool.BaseTool{
		&lsTool{base: base},
		&readTool{base: base},
		&writeTool{base: base},
		&grepTool{base: base},
		&findTool{base: base},
		&scriptTool{base: base},
	}

	return tools, nil
}

// createAgent builds the eino agent from the session's tools.
//
// Mapping from old code: this replaces createAgent in internal/worker/agent.go.
// The key difference is that the summarization middleware is injected via
// Config.AgentMiddlewares (set up in New) rather than being created inline.
//
// SessionID threading: the context is wrapped with sessionID via
// withSessionID so that the summarization middleware's callbacks
// (CompressContext, OnCompress) can extract it via getSessionIDFromContext.
// This is necessary because the middleware does not receive sessionID
// natively — it only sees the context passed through the agent execution.
func (h *helpers) createAgent(ctx context.Context, sessionID string, turnID string, tools []tool.BaseTool) (adk.Agent, error) {
	// Validate required dependencies
	if h.deps.ChatModel == nil {
		return nil, fmt.Errorf("createAgent: ChatModel is nil (LLM not configured)")
	}
	if h.deps.SystemPrompt == "" {
		return nil, fmt.Errorf("createAgent: SystemPrompt is empty")
	}

	// Inject sessionID into context for summarization middleware callbacks.
	sid, parseErr := uuid.Parse(sessionID)
	if parseErr != nil {
		return nil, fmt.Errorf("createAgent: invalid session ID %q: %w", sessionID, parseErr)
	}
	ctx = withSessionID(ctx, sid)

	// Build handlers list, filtering out nil middleware
	var handlers []adk.ChatModelAgentMiddleware
	if h.summarizeMW != nil {
		handlers = append(handlers, h.summarizeMW)
	}

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        fmt.Sprintf("session-%s", sessionID),
		Description: "RTC Agent session handler",
		Instruction: h.deps.SystemPrompt,
		Model:       h.deps.ChatModel,
		Handlers:    handlers,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: tools,
				UnknownToolsHandler: func(ctx context.Context, name, input string) (string, error) {
					return fmt.Sprintf("<system>\nExit with code: 404, tool %s not found\n</system>", name), nil
				},
				ExecuteSequentially: true,
				ToolCallMiddlewares: nil,
			},
			ReturnDirectly:     nil,
			EmitInternalEvents: false,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("createAgent: create chat model agent: %w", err)
	}

	return agent, nil
}

// publishEvent handles flattened events from turn-agent's event stream.
//
// This is called once per event from the agent's execution. The pkg handles
// all eino stream consumption internally — this callback receives already-
// flattened events (chunks, end markers, complete messages, errors).
//
// Mapping from old code: this replaces the ~185-line handleEvents function
// in internal/worker/session_actor.go. The old code consumed eino's stream
// objects directly; the new code receives pre-processed events and handles
// them with a simple switch on EventKind.
//
// Streaming state (tracking the current streaming message row) is maintained
// per-turn via the streamState map, keyed by turnID.
func (h *helpers) publishEvent(ctx context.Context, sessionID string, turnID string, event *turnagent.Event) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("publishEvent: invalid session ID %q: %w", sessionID, err)
	}
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("publishEvent: invalid turn ID %q: %w", turnID, err)
	}

	switch event.Kind {
	case turnagent.EventKindStreamChunk:
		return h.handleStreamChunk(ctx, sid, tid, event)
	case turnagent.EventKindStreamEnd:
		return h.handleStreamEnd(ctx, sid, tid, event)
	case turnagent.EventKindMessage:
		return h.handleMessage(ctx, sid, tid, event)
	case turnagent.EventKindError:
		return h.handleEventError(ctx, sid, tid, event)
	default:
		h.logIfEnabled(ctx, "publishEvent.unknown_kind", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"kind":       string(event.Kind),
		})
		return nil
	}
}
