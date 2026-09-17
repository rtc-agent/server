package turnagent

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
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
//
// lastMessage is the final assistant message produced by the turn (may be nil
// if the turn produced no assistant message). Used by Sub Agent support to
// pass the sub session's result to the parent session.
type CompleteTurnFunc func(ctx context.Context, sessionID string, turnID string, lastMessage *Message) error

// InterruptTurnFunc is called when the turn pauses due to an eino interrupt
// (typically raised by a tool calling tool.StatefulInterrupt).
//
// interruptID is the eino-assigned ID of the root interrupt context — the
// deepest tool that actually raised the interrupt. interruptInfo is the
// opaque info passed to StatefulInterrupt (e.g., rtcInterruptInfo); its
// concrete type is application-defined.
//
// For batch interrupts (multiple tools interrupting simultaneously),
// allInterruptContexts contains all interrupt contexts. Each context has:
//   - ID: eino's internal interrupt ID (used as key in ResumeParams.Targets)
//   - Info: the opaque info passed to StatefulInterrupt
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
type InterruptTurnFunc func(ctx context.Context, turnID string, interruptID string, interruptInfo any, allInterruptContexts []*InterruptContext) error

// InterruptContext represents a single interrupt context from eino.
// Used for batch interrupt tracking where multiple tools may interrupt simultaneously.
type InterruptContext struct {
	// ID is eino's internal interrupt ID (used as key in ResumeParams.Targets)
	ID string
	// Info is the opaque info passed to StatefulInterrupt (e.g., rtcInterruptInfo)
	Info any
}

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
