package agent

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/agent/command"
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
	// subAgentInterruptState: Sub agent tool's interrupt state
	// subAgentInterruptInfo:  Sub agent tool's interrupt info
	//
	// Mapping from old code: identical to the gob.Register calls in
	// internal/worker/tools.rtc.go's init().
	gob.Register(rtcInterruptState{})
	gob.Register(rtcInterruptInfo{})
	gob.Register(subAgentInterruptState{})
	gob.Register(subAgentInterruptInfo{})
}

// formatToolCallOutput formats a tool call's output for injection into the
// agent's context. Mirrors the old formatToolCallOutput in context.go.
// Output templates are centralised in prompts/outputs/ and accessed via
// the format* helpers in output_prompts.go.
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
		// If the output is a JSON-encoded string (e.g. "{\"logs\":...}"),
		// unmarshal it to return the raw JSON object. This eliminates one
		// layer of escape in the LLM context:
		//   Before: "{\"logs\":[\"hello\"]}"  (escaped string)
		//   After:  {"logs":["hello"]}        (raw JSON object)
		if unquoted, err := unquoteJSONString(output); err == nil {
			return unquoted
		}
		return output
	case protocol.RtcStatusFailed:
		return formatToolError(toolCall.ToolName, output)
	case protocol.RtcStatusTimeout:
		return formatToolTimeout(toolCall.ToolName)
	case protocol.RtcStatusRejected:
		return formatToolRejected(toolCall.ToolName)
	default:
		return formatToolPending(toolCall.ToolName, status)
	}
}

// unquoteJSONString checks if s is a JSON-encoded string (starts with ")
// and, if so, unmarshals it to return the raw string value.
// This is used to unwrap one layer of JSON string encoding:
//
//	`"{\"key\":\"value\"}"` → `{"key":"value"}`
//
// Returns an error if s is not a valid JSON string.
func unquoteJSONString(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' {
		return "", fmt.Errorf("not a JSON string")
	}
	var result string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return "", err
	}
	return result, nil
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
		&editTool{base: base},
		&grepTool{base: base},
		&findTool{base: base},
		&scriptTool{base: base},
		// askUserTool needs its own rtcToolBase with a custom result formatter
		// that assembles "User has answered your questions: ..." from the JSON
		// answers submitted by the client.
		&askUserTool{base: &rtcToolBase{
			session:      session,
			helpers:      h,
			turnID:       tid,
			formatResult: FormatAskUserResult,
		}},
		&todoWriteTool{helper: h, session: session, turnID: tid},
		// subAgentTool enables LLM to create sub agent sessions for task decomposition.
		&subAgentTool{
			session: session,
			helpers: h,
			turnID:  tid,
		},
		// listSubAgentTool lists all running sub agent sessions.
		&listSubAgentTool{
			session: session,
			helpers: h,
			turnID:  tid,
		},
		// getSubAgentMessageTool queries the last message from a specific sub agent.
		&getSubAgentMessageTool{
			session: session,
			helpers: h,
			turnID:  tid,
		},
		// stopSubAgentTool stops a running sub agent and all its descendants.
		&stopSubAgentTool{
			session: session,
			helpers: h,
			turnID:  tid,
		},
		// sendMessageToSubAgentTool sends a message to an async sub-agent session.
		&sendMessageToSubAgentTool{
			session: session,
			helpers: h,
			turnID:  tid,
		},
	}

	// Add Session Memory tools
	if saveMemoryTool := h.createSaveSessionMemoryTool(session, tid); saveMemoryTool != nil {
		tools = append(tools, saveMemoryTool)
	}
	if listMemoriesTool := h.createListSessionMemoriesTool(session, tid); listMemoriesTool != nil {
		tools = append(tools, listMemoriesTool)
	}
	if searchMemoryTool := h.createSearchMemoryTool(session, tid); searchMemoryTool != nil {
		tools = append(tools, searchMemoryTool)
	}

	// Add User Memory tools
	if saveUserMemoryTool := h.createSaveUserMemoryTool(session, tid); saveUserMemoryTool != nil {
		tools = append(tools, saveUserMemoryTool)
	}
	if updateUserMemoryTool := h.createUpdateUserMemoryTool(session, tid); updateUserMemoryTool != nil {
		tools = append(tools, updateUserMemoryTool)
	}
	if deleteUserMemoryTool := h.createDeleteUserMemoryTool(session, tid); deleteUserMemoryTool != nil {
		tools = append(tools, deleteUserMemoryTool)
	}
	if listUserMemoryTool := h.createListUserMemoryTool(session, tid); listUserMemoryTool != nil {
		tools = append(tools, listUserMemoryTool)
	}

	// Slash-command framework: collect tools from ALL registered commands.
	//
	// Static registration: tools are always available to the LLM, regardless
	// of command activation state. This eliminates the need to track activation
	// state across interrupt/resume cycles, solving the "tools disappear on resume"
	// problem.
	//
	// The LLM is guided to use these tools appropriately by TriggerPrompt
	// injections (e.g., "only call createLoop after user confirms").
	if h.deps.CommandRegistry != nil {
		cmdCtx := command.Context{
			Context:   ctx,
			SessionID: sid,
			TurnID:    tid,
		}
		tools = append(tools, h.deps.CommandRegistry.CollectAllTools(cmdCtx)...)
	}

	// Wrap all tools with error handler: convert tool errors to string results
	// so the LLM can see error messages and self-correct, instead of terminating
	// the entire turn with NodeRunError. Interrupt errors are preserved (not wrapped).
	// Use <error> XML tags to clearly delimit the error, following Claude Code's
	// convention — this helps the LLM distinguish errors from normal output.
	//
	// Additionally, log tool call failures server-side so we can diagnose
	// issues without relying on LLM to surface them.
	errorHandler := func(ctx context.Context, err error) string {
		return formatErrorWrapper(err.Error())
	}
	wrappedTools := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		// Wrap layers (outer to inner): error handler → normalizer → logger → tool
		// 1. Logger: records tool call failures server-side
		logged := &toolCallLogger{inner: t, helpers: h, sessionID: sid, turnID: tid}
		// 2. Normalizer: preprocesses empty/null/whitespace arguments to "{}"
		normalized := &toolCallArgumentsNormalizer{inner: logged}
		// 3. Error handler: converts errors to user-friendly strings
		wrappedTools[i] = utils.WrapToolWithErrorHandler(normalized, errorHandler)
	}

	h.logger.Info(ctx, "createTools.done", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
		"tool_count": len(wrappedTools),
	})

	return wrappedTools, nil
}

// toolCallLogger wraps a tool to log invocations and errors server-side.
// This provides observability into tool failures that would otherwise only
// be visible to the LLM (via the error handler wrapper). Delegates all
// interface methods to the inner tool.
//
// Implements InvokableTool by delegating to the inner tool. If the inner
// tool is not invokable (BaseTool-only), InvokableRun returns an error
// instead of panicking — this protects against future tool types that
// don't implement InvokableTool.
type toolCallLogger struct {
	inner     tool.BaseTool
	helpers   *helpers
	sessionID uuid.UUID
	turnID    uuid.UUID
}

func (l *toolCallLogger) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return l.inner.Info(ctx)
}

// toolName extracts the tool name via Info(). Returns "unknown" on error.
func (l *toolCallLogger) toolName(ctx context.Context) string {
	if info, _ := l.inner.Info(ctx); info != nil {
		return info.Name
	}
	return "unknown"
}

func (l *toolCallLogger) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	// Safe type assertion: if inner is not InvokableTool, return an error
	// instead of panicking. This protects against future tool types that
	// only implement BaseTool (e.g., read-only schema providers).
	invokable, ok := l.inner.(tool.InvokableTool)
	if !ok {
		name := l.toolName(ctx)
		return formatErrorWrapper("tool " + name + " does not support invocation"), nil
	}

	toolName := l.toolName(ctx)

	result, err := invokable.InvokableRun(ctx, argumentsInJSON, opts...)
	if err != nil {
		l.helpers.logger.Warn(ctx, "tool.call_failed", map[string]any{
			"tool_name":  toolName,
			"session_id": l.sessionID.String(),
			"turn_id":    l.turnID.String(),
			"error":      err.Error(),
		})
	}
	return result, err
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
	h.logger.Info(ctx, "createAgent.start", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
		"tool_count": len(tools),
	})
	// Resolve the effective system prompt.
	// If worker.system_prompt is set in YAML config, it completely overrides
	// the embedded default (backward-compatible). Otherwise, assemble from
	// the section-based embedded prompts.
	systemPrompt := h.deps.SystemPrompt
	if systemPrompt == "" {
		built, err := BuildDefaultSystemPrompt()
		if err != nil {
			return nil, fmt.Errorf("createAgent: failed to build default system prompt: %w", err)
		}
		systemPrompt = built
	}

	// Validate required dependencies
	if h.deps.ChatModel == nil {
		return nil, fmt.Errorf("createAgent: ChatModel is nil (LLM not configured)")
	}
	if systemPrompt == "" {
		return nil, fmt.Errorf("createAgent: system prompt is empty after resolution")
	}

	// Inject sessionID into context for summarization middleware callbacks.
	sid, parseErr := uuid.Parse(sessionID)
	if parseErr != nil {
		return nil, fmt.Errorf("createAgent: invalid session ID %q: %w", sessionID, parseErr)
	}
	ctx = withSessionID(ctx, sid)

	// Build handlers list, filtering out nil middleware
	// Order matters: merge assistant first (merges adjacent assistant messages),
	// then summarize (context compression)
	var handlers []adk.ChatModelAgentMiddleware
	if h.mergeAssistantMW != nil {
		handlers = append(handlers, h.mergeAssistantMW)
	}
	if h.summarizeMW != nil {
		handlers = append(handlers, h.summarizeMW)
	}

	session, err := h.deps.SessionRepo.GetByID(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("createAgent: unable to get session ID %q: %w", sessionID, err)
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
				// attempt starts at 1 for the first retry.
				// Cap shift at 30 to prevent overflow (2^30 * baseDelay ≈ 17 min
				// for a 1s base). Without the cap, attempt > 63 would wrap to
				// negative durations.
				shift := uint(attempt - 1)
				if shift > 30 {
					shift = 30
				}
				return baseDelay * time.Duration(1<<shift)
			}
		}
	}

	// System prompt and AgentPrompt are now injected via loadMessages pipeline
	// (injectSystemAndAgentPrompt) to ensure consistent system array structure
	// between GenInput and GenResume paths.
	//
	// BUG 11 fix: Instruction is set to empty string "" instead of " " (space).
	// Previously, eino added Instruction as system[0] in GenInput but not in GenResume,
	// causing system array structure inconsistency and cache invalidation.
	// Empty string prevents eino from adding any system message for Instruction.
	instruction := ""
	h.logger.Info(ctx, "createAgent.instruction_debug", map[string]any{
		"session_id":           sessionID,
		"instruction_length":   len(instruction),
		"instruction_empty":    instruction == "",
		"system_prompt_length": len(systemPrompt),
		"agent_prompt_length":  len(session.AgentPrompt),
		"note":                 "Instruction minimized to empty string (BUG 11 fix)",
	})

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:             fmt.Sprintf("session-%s", sessionID),
		Description:      "RTC Agent session handler",
		Instruction:      instruction,
		Model:            h.deps.ChatModel,
		Handlers:         handlers,
		ModelRetryConfig: retryConfig,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: tools,
				UnknownToolsHandler: func(ctx context.Context, name, input string) (string, error) {
					return formatUnknownTool(name), nil
				},
				ExecuteSequentially: true,
			},
			ReturnDirectly:     nil,
			EmitInternalEvents: false,
		},
	})
	if err != nil {
		h.logger.Info(ctx, "createAgent.failed", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"error":      err.Error(),
		})
		return nil, fmt.Errorf("createAgent: create chat model agent: %w", err)
	}

	h.logger.Info(ctx, "createAgent.done", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
	})
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
		h.logger.Info(ctx, "publishEvent.unknown_kind", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"kind":       string(event.Kind),
		})
		return nil
	}
}
