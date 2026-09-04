package turnagent

import (
	"context"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/tool"
	"go.opentelemetry.io/otel/trace"
)

// =============================================================================
// Turn ownership
// =============================================================================
//
// turn-agent owns the turn lifecycle. The API layer publishes bare Work items
// (just {kind, sessionID}) into rtc-queue; turn-agent is responsible for
// creating turns (for submit) and locating existing turns (for resume). Both
// operations are delegated to the application via callbacks so the package
// stays free of DB schema dependencies.
//
// The application is expected to maintain the invariant: at most one active
// turn per session. CreateTurn is called exactly once per turn, on submit.
// LookupTurn is called on resume to find the turn to continue.

// CreateTurnFunc is called once, at the start of a submit work, to allocate a
// new turn. The implementation typically generates a turn UUID, inserts a row
// into the turns table (status="created" or similar), and returns the turnID.
//
// workID is the rtc-queue Work.ID of the triggering work item. Implementations
// SHOULD use it as an idempotency key: if the same work is retried (e.g., after
// a worker crash), CreateTurn MUST return the same turnID rather than creating
// a duplicate. A common pattern is to persist workID alongside the turn row
// and upsert on (sessionID, workID).
//
// The returned turnID is then passed to BeginTurn (to mark it as "running")
// and to every subsequent lifecycle / data callback for the duration of the
// turn — including across interrupts and resumes on different workers.
//
// Errors from CreateTurn abort the work: Agent.Process returns the error, and
// rtc-queue leaves the work in "processing" status for admin recovery.
type CreateTurnFunc func(ctx context.Context, sessionID string, workID string) (turnID string, err error)

// LookupTurnFunc is called at the start of a resume work to find the existing
// turn for the given session. The implementation typically queries the turns
// table for the active turn of this session (e.g., WHERE session_id = ? AND
// status IN ('interrupted', 'running')).
//
// workID is the rtc-queue Work.ID of the triggering work item. Reserved for
// future use (e.g., idempotent resume); implementations may ignore it.
//
// The returned turnID is then passed to ResumeTurn (to mark it as "running"
// again) and to subsequent callbacks.
//
// If no active turn exists (e.g., stale resume work after the turn was
// already completed or cancelled), the implementation should return an error.
// Agent.Process will propagate the error; rtc-queue leaves the work in
// "processing" status for admin recovery.
//
// IMPLEMENTATION NOTE: LookupTurn MUST be able to find turns in "running"
// state — a previous worker may have crashed after BeginTurn but before
// reaching a terminal callback. The caller is expected to treat such a turn
// as recoverable and transition it back to "running" via ResumeTurn.
type LookupTurnFunc func(ctx context.Context, sessionID string, workID string) (turnID string, err error)

// =============================================================================
// Turn state transitions
// =============================================================================
//
// These callbacks are called by Agent.Process at well-defined points in the
// turn's lifecycle. They are the ONLY mechanism through which turn state
// transitions occur. Application code implements them to persist the
// transition (DB updates, pub/sub notifications, etc.).
//
// START callback errors (BeginTurn, ResumeTurn) abort the work: the Agent
// returns the error and rtc-queue leaves the work in "processing" status for
// admin recovery.
//
// TERMINAL callback errors (CompleteTurn, InterruptTurn, FailTurn,
// CancelTurn) are LOGGED but do NOT abort the work: the eino TurnLoop has
// already exited and the turn has reached a terminal state. Process() returns
// nil so rtc-queue marks the work as complete. The implementation should
// treat terminal-callback failure as a DB inconsistency to be repaired by
// admin tooling or a reconciliation job, not as a reason to retry the turn.

// BeginTurnFunc is called after CreateTurn, to mark the freshly created turn
// as "running".
//
// Called exactly once per turn. The same turnID will be passed to exactly
// one of the terminal callbacks (Complete/Interrupt/Fail/Cancel) before the
// turn ends.
//
// CONTRACT: the implementation MUST persist the "running" state AND publish
// a turn.updated event so the frontend observes the transition. turn-agent
// does not emit pub/sub events directly — lifecycle callbacks are the sole
// integration point for frontend notifications.
type BeginTurnFunc func(ctx context.Context, turnID string) error

// CompleteTurnFunc is called when the turn finishes cleanly: the agent
// produced its final response with no interrupt and no error. The eino
// checkpoint has already been deleted by eino's TurnLoop on clean exit.
//
// CONTRACT: the implementation MUST persist the "completed" state AND publish
// a turn.updated event.
//
// If this callback returns an error, the error is logged and Process() still
// returns nil — the turn has reached its terminal state.
type CompleteTurnFunc func(ctx context.Context, turnID string) error

// InterruptTurnFunc is called when the turn pauses due to an eino interrupt
// (typically raised by a tool calling tool.StatefulInterrupt).
//
// interruptID is the eino-assigned ID of the root interrupt context — the
// deepest tool that actually raised the interrupt. interruptInfo is the
// opaque info passed to StatefulInterrupt (e.g., rtcInterruptInfo); its
// concrete type is application-defined.
//
// The eino checkpoint has already been persisted to the CheckpointStore.
// The implementation should persist the "interrupted" state and notify the
// frontend if needed.
//
// CONTRACT: the implementation MUST persist the "interrupted" state AND
// publish a turn.updated event.
//
// After this callback returns (even with an error), Agent.Process returns
// nil, rtc-queue's Worker calls Complete on the work, and the session lock
// is released. The application is responsible for publishing a Resume work
// item to rtc-queue when the external event resolves (e.g., when
// SubmitRtcResult is called).
//
// If this callback returns an error, the error is logged and Process() still
// returns nil — the turn has reached its terminal state.
type InterruptTurnFunc func(ctx context.Context, turnID string, interruptID string, interruptInfo any) error

// ResumeTurnFunc is called after LookupTurn, to mark an interrupted turn as
// "running" again before eino's TurnLoop re-enters the tool.
//
// Called exactly once per resume. After this callback, Agent.Process runs
// the eino TurnLoop, which loads the checkpoint and re-enters the tool that
// previously interrupted.
//
// CONTRACT: the implementation MUST persist the "running" state AND publish
// a turn.updated event.
type ResumeTurnFunc func(ctx context.Context, turnID string) error

// FailTurnFunc is called when the turn ends due to an unexpected error
// (agent failure, tool error, etc.). The eino checkpoint may or may not be
// present depending on where the failure occurred.
//
// CONTRACT: the implementation MUST persist the "failed" state (with the
// error message) AND publish a turn.updated event.
//
// If this callback returns an error, the error is logged and the original
// turn error is still returned to rtc-queue so the work stays in
// "processing" for admin recovery.
type FailTurnFunc func(ctx context.Context, turnID string, err error) error

// CancelTurnFunc is called when the turn is cancelled via rtc-queue's Cancel
// admin operation. reason carries the CancelMessage.Reason from the publisher.
//
// CONTRACT: the implementation MUST persist the "cancelled" state AND publish
// a turn.updated event.
//
// If this callback returns an error, the error is logged and Process() still
// returns nil — the turn has reached its terminal state.
type CancelTurnFunc func(ctx context.Context, turnID string, reason string) error

// =============================================================================
// Data callbacks
// =============================================================================
//
// All data callbacks use the pkg's own types (Message, Event). The upper
// application never writes code that consumes eino's flow primitives (no
// stream consumption loops, no AgentEvent inspection, no IsStreaming
// branching). It only implements business logic: load messages from the DB,
// persist chunks, deliver complete messages, route events.

// LoadMessagesFunc loads the conversation history for a session. Called by
// eino's GenInput path (fresh turns only — eino does not call GenInput when
// resuming from a checkpoint).
//
// The returned messages become the agent's input for this turn. The
// implementation should include the full history (system prompt, previous
// turns, the new user message that triggered this work, etc.). turn-agent
// does not carry per-message identity in WorkPayload; LoadMessages is the
// place where the application decides what the agent sees.
//
// If the returned slice is empty, a warning is logged and the agent runs
// with empty input — the implementation should avoid this in normal
// operation.
type LoadMessagesFunc func(ctx context.Context, sessionID string) ([]*Message, error)

// CreateToolsFunc creates the tool list for a session's turn.
//
// turnID is provided so the implementation can inject it into the tool's
// context (e.g., so tool.InvokableRun can read it via context.Value). Tools
// often need turnID for logging, DB writes, or RTC state lookup.
//
// IMPORTANT: the tool configuration must be identical across interrupts and
// resumes for the same turn — eino's checkpoint encodes tool-call state that
// depends on the tool layout. The implementation should return tools
// deterministically for a given sessionID.
//
// NOTE: this callback returns eino's tool.BaseTool. The upper application
// references this eino type but does not write eino flow code.
type CreateToolsFunc func(ctx context.Context, sessionID string, turnID string) ([]tool.BaseTool, error)

// CreateAgentFunc builds the agent from the session's tools.
//
// turnID is provided for the same reason as in CreateTools: the agent (or
// middleware wrapped around it) may need turnID for tracing or context
// injection.
//
// The agent is typically a *adk.ChatModelAgent wrapping the tools with a
// chat model. The implementation may also wire in middleware (e.g., the
// summarization middleware from this package) via the agent config.
//
// NOTE: this callback returns eino's adk.Agent. The upper application
// references this eino type but does not write eino flow code.
type CreateAgentFunc func(ctx context.Context, sessionID string, turnID string, tools []tool.BaseTool) (adk.Agent, error)

// PublishEventFunc is called once per flattened event from the agent's
// execution.
//
// The pkg handles ALL eino stream consumption internally:
//   - Streaming responses are consumed and surfaced as a sequence of
//     EventKindStreamChunk events, terminated by EventKindStreamEnd.
//   - Non-streaming responses are surfaced as a single EventKindMessage event.
//   - Event-level errors are surfaced as EventKindError.
//
// The upper application implements only business logic:
//   - For EventKindStreamChunk: append the chunk to the appropriate message
//     row (tracking its own state for markdown vs. thinking rows).
//   - For EventKindStreamEnd: finalize the streaming message rows.
//   - For EventKindMessage: persist the complete message.
//   - For EventKindError: log or surface the error.
//
// sessionID and turnID are provided so the implementation can route the
// event to the correct subscribers / DB rows. Errors abort the event stream
// for this turn.
type PublishEventFunc func(ctx context.Context, sessionID string, turnID string, event *Event) error

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
}

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
