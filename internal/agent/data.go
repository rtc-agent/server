package agent

import (
	"context"
	"encoding/gob"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/google/uuid"
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

	// Build retry config if configured
	var retryConfig *adk.ModelRetryConfig
	if h.deps.LLMConfig.RetryMaxAttempts > 0 {
		retryConfig = &adk.ModelRetryConfig{
			MaxRetries: h.deps.LLMConfig.RetryMaxAttempts,
		}
		// Custom backoff function if base delay is configured
		if h.deps.LLMConfig.RetryBaseDelay > 0 {
			baseDelay := h.deps.LLMConfig.RetryBaseDelay
			retryConfig.BackoffFunc = func(ctx context.Context, attempt int) time.Duration {
				// Exponential backoff: baseDelay * 2^(attempt-1)
				// attempt starts at 1 for the first retry
				return baseDelay * time.Duration(1<<uint(attempt-1))
			}
		}
	}

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:             fmt.Sprintf("session-%s", sessionID),
		Description:      "RTC Agent session handler",
		Instruction:      h.deps.SystemPrompt,
		Model:            h.deps.ChatModel,
		Handlers:         handlers,
		ModelRetryConfig: retryConfig,
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
