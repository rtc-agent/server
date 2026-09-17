package turnagent

import (
	"context"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"go.opentelemetry.io/otel/trace"
)

// =============================================================================
// Config
// =============================================================================

// Config holds the callbacks and dependencies for an Agent.
//
// Required fields:
//   - Turn ownership: CreateTurn, LookupTurn
//   - Lifecycle: BeginTurn, CompleteTurn, InterruptTurn, ResumeTurn, FailTurn, CancelTurn
//   - Data: LoadMessages, CreateTools, CreateAgent, PublishEvent
//   - CheckpointStore
//
// Optional fields:
//   - DeriveCheckpointID (defaults to "turnagent:session:<sessionID>")
//   - AgentMiddlewares (made available to CreateAgent for injection)
//   - Callbacks (eino callback handlers for LLM/tool-level observability)
//   - Logger, Tracer, Metrics (pkg-level observability)
//   - EnableLLMLogging
type Config struct {
	// ----- Turn ownership (required) -----

	CreateTurn CreateTurnFunc // allocates a new turn on submit
	LookupTurn LookupTurnFunc // finds the active turn on resume

	// ----- Turn state transitions (required) -----

	BeginTurn     BeginTurnFunc
	CompleteTurn  CompleteTurnFunc
	InterruptTurn InterruptTurnFunc
	ResumeTurn    ResumeTurnFunc
	FailTurn      FailTurnFunc
	CancelTurn    CancelTurnFunc

	// ----- Data (required) -----

	LoadMessages LoadMessagesFunc
	CreateTools  CreateToolsFunc
	CreateAgent  CreateAgentFunc
	PublishEvent PublishEventFunc

	// ----- Checkpoint (required) -----

	// CheckpointStore is the eino checkpoint store. Typically backed by Redis
	// with a TTL (e.g., 24h). The same store must be used across all workers
	// so that a resume on a different node can load the checkpoint written by
	// the interrupting node.
	//
	// NOTE: this is eino's adk.CheckPointStore type. The upper application
	// references it but does not write eino flow code.
	CheckpointStore adk.CheckPointStore

	// DeriveCheckpointID maps a sessionID to the eino checkpoint key. Optional;
	// defaults to "turnagent:session:<sessionID>".
	//
	// The returned ID MUST be stable across process restarts and workers — it
	// is the key under which the eino checkpoint is stored, and a resume on a
	// different worker must find the same checkpoint.
	DeriveCheckpointID func(sessionID string) string

	// ----- Middleware (optional) -----

	// AgentMiddlewares are available to CreateAgent for injection into the
	// agent it constructs. The Agent itself does not apply these — they are
	// passed through so CreateAgent can wire them into adk.NewChatModelAgent.
	//
	// Storing them on Config (rather than requiring CreateAgent to close over
	// them) makes middleware injection declarative and testable.
	//
	// Typical use:
	//
	//	cfg := turnagent.Config{
	//	    AgentMiddlewares: []adk.ChatModelAgentMiddleware{summarizeMW},
	//	    CreateAgent: func(ctx, sessionID, turnID string, tools []tool.BaseTool) (adk.Agent, error) {
	//	        return adk.NewChatModelAgent(ctx, adk.ChatModelAgentConfig{
	//	            Tools: tools,
	//	            Middlewares: cfg.AgentMiddlewares,  // closed-over
	//	        })
	//	    },
	//	}
	AgentMiddlewares []adk.ChatModelAgentMiddleware

	// ----- eino callbacks (optional) -----

	// Callbacks is a list of eino callback handlers injected into the context
	// of each turn. The pkg calls callbacks.InitCallbacks at the start of
	// GenInput with an empty RunInfo, mirroring turn-loop's session-level
	// injection — per-component RunInfo is filled in by eino as it walks the
	// agent tree.
	//
	// Use this to wire in application-level tracing / metrics handlers
	// (e.g., cozeloop, langfuse, OpenTelemetry) for LLM and tool calls.
	// If nil or empty, no handlers are injected.
	//
	// NOTE: this field references eino's callbacks.Handler type. The upper
	// application references this eino type but does not write eino flow code.
	Callbacks []callbacks.Handler

	// ----- Observability (optional) -----

	// Logger provides structured logging for the pkg's lifecycle events.
	// Optional — if nil, no logs are emitted.
	Logger Logger

	// Tracer provides OpenTelemetry distributed tracing. Optional — if nil,
	// no spans are created. The pkg creates one root span per Process()
	// invocation.
	Tracer trace.Tracer

	// Metrics provides pluggable metrics collection. Optional — if nil, no
	// metrics are emitted. The pkg calls RecordTurn and RecordInterrupt
	// automatically; RecordLLMCall and RecordCheckpoint must be called by
	// the application (typically via eino callbacks or a CheckpointStore
	// decorator).
	Metrics Metrics

	// EnableLLMLogging is a hint to the application that verbose LLM I/O
	// logging is desired. The pkg itself does not log LLM payloads — the
	// application should wire an appropriate callbacks.Handler via Config.Callbacks
	// to capture LLM calls. This flag is surfaced via the "agent.new" debug
	// log so the application can conditionally enable more verbose callback
	// handlers.
	EnableLLMLogging bool

	// Cancel controls how turn cancellation behaves when a CancelMessage is
	// received from rtc-queue.
	//
	// If Cancel.GracePeriod is zero (default), cancellation is immediate: the
	// agent is aborted as soon as possible via loop.Stop(WithImmediate()).
	// Use this for admin cancels where responsiveness matters more than output
	// coherence.
	//
	// If Cancel.GracePeriod is positive, cancellation is graceful: the agent
	// runs until it reaches a safe point (after a chat-model call or tool call),
	// with the given grace period as a hard upper bound before escalating to
	// immediate. Use this for user-initiated stops where a half-generated
	// response would be visible to the frontend.
	//
	// The safe point used is AfterChatModel | AfterToolCalls, matching the
	// behavior of adk.WithGracefulTimeout.
	Cancel CancelConfig

	// ----- Reactive compact (optional) -----

	// RecoverFromPromptTooLong is called when the LLM returns a prompt-too-long
	// error (e.g., Claude's "prompt is too long" or OpenAI's
	// "context_length_exceeded"). The implementation should compress the
	// conversation history (e.g., generate a summary, aggressively clear tool
	// results, or soft-delete old messages) so the next attempt fits within the
	// LLM's context window.
	//
	// After this callback returns nil, Agent.Process creates a new TurnLoop and
	// retries. If the retry also fails with prompt-too-long, the callback is
	// called again (up to MaxReactiveCompactAttempts total attempts).
	//
	// If nil, prompt-too-long errors fall through to FailTurn (no recovery).
	RecoverFromPromptTooLong RecoverFromPromptTooLongFunc

	// MaxReactiveCompactAttempts is the maximum number of recovery attempts
	// before giving up and calling FailTurn. Default: 3.
	MaxReactiveCompactAttempts int

	// InsertFeedbackMessage inserts a feedback message into the conversation
	// (cross-package bridge, used by the reactive compact path).
	// Parameters: ctx, sessionID, turnID, category, title, message string, retryable bool, rawError string.
	InsertFeedbackMessage func(ctx context.Context, sessionID, turnID, category, title, message string, retryable bool, rawError string) error

	// ----- Explicit compact (optional) -----

	// CompactContext is called when an explicit compact work item
	// (WorkKindCompact) is processed. Unlike RecoverFromPromptTooLong (which
	// is reactive and triggered by prompt-too-long errors), CompactContext
	// is user-initiated via the /compact command.
	//
	// The implementation should load messages, compress them (summarize +
	// persist), and push stats to the Live channel.
	//
	// If nil, compact work items are silently ignored (no-op).
	CompactContext CompactContextFunc
}

// CompactContextFunc is the callback for explicit compact work items.
// customInstruction is an optional user-provided instruction that overrides
// the default compression prompt (nil means use default).
type CompactContextFunc func(ctx context.Context, sessionID string, customInstruction *string) error

// RecoverFromPromptTooLongFunc is the callback for reactive compact recovery.
// Called with the session ID and the current attempt number (1-based).
// The implementation should use the attempt number to escalate the recovery
// strategy (e.g., attempt 1: aggressive compact, attempt 2: more aggressive,
// attempt 3: soft-delete old messages).
//
// The caller (Agent.Process) tracks the attempt number across retries within a
// single Process() invocation. The counter is NOT shared across Process() calls.
type RecoverFromPromptTooLongFunc func(ctx context.Context, sessionID string, attempt int) error

// CancelConfig controls turn cancellation behavior.
type CancelConfig struct {
	// GracePeriod controls whether turn cancellation is immediate or graceful.
	//
	// If zero (default), cancellation is immediate: the agent is aborted as
	// soon as possible via loop.Stop(WithImmediate()). Use this for admin
	// cancels where responsiveness matters more than output coherence.
	//
	// If positive, cancellation is graceful: the agent runs until it reaches
	// a safe point (after a chat-model call or tool call), with the given
	// grace period as a hard upper bound before escalating to immediate.
	// Use this for user-initiated stops where a half-generated response
	// would be visible to the frontend.
	//
	// The safe point used is AfterChatModel | AfterToolCalls, matching the
	// behavior of adk.WithGracefulTimeout.
	GracePeriod time.Duration
}

// validate checks that required fields are set.
func (c Config) validate() error {
	// Turn ownership
	switch {
	case c.CreateTurn == nil:
		return errMissing("CreateTurn")
	case c.LookupTurn == nil:
		return errMissing("LookupTurn")
	}
	// Lifecycle
	switch {
	case c.BeginTurn == nil:
		return errMissing("BeginTurn")
	case c.CompleteTurn == nil:
		return errMissing("CompleteTurn")
	case c.InterruptTurn == nil:
		return errMissing("InterruptTurn")
	case c.ResumeTurn == nil:
		return errMissing("ResumeTurn")
	case c.FailTurn == nil:
		return errMissing("FailTurn")
	case c.CancelTurn == nil:
		return errMissing("CancelTurn")
	}
	// Data
	switch {
	case c.LoadMessages == nil:
		return errMissing("LoadMessages")
	case c.CreateTools == nil:
		return errMissing("CreateTools")
	case c.CreateAgent == nil:
		return errMissing("CreateAgent")
	case c.PublishEvent == nil:
		return errMissing("PublishEvent")
	case c.CheckpointStore == nil:
		return errMissing("CheckpointStore")
	}
	return nil
}
