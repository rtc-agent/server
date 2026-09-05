// Package turnagent provides a distributed, turn-based agent execution framework
// built on eino's TurnLoop and rtc-queue.
//
// # Model
//
// A "turn" is one unit of agent execution, spanning from a user message submission
// to the agent's final response. A turn may be interrupted (e.g., by an RTC tool
// waiting for client-side execution) and later resumed from a checkpoint. A turn
// can span multiple rtc-queue Work items and multiple worker processes, but its
// lifecycle is managed exclusively by Agent.Process().
//
// # Design
//
// The Agent is stateless: it receives a Work item from rtc-queue, runs one eino
// TurnLoop iteration, manages the turn's lifecycle via Config callbacks, and
// returns. All persistent state lives in the eino checkpoint store and the
// application's database.
//
// The Agent drives the turn lifecycle through six callbacks:
//
//	BeginTurn     → running       (called once at the start of a fresh turn)
//	ResumeTurn    → running       (called when an interrupted turn resumes)
//	CompleteTurn  → completed     (called on clean exit)
//	InterruptTurn → interrupted   (called when eino raises InterruptError)
//	FailTurn      → failed        (called on unexpected error)
//	CancelTurn    → cancelled     (called when rtc-queue Cancel is received)
//
// Application code implements these callbacks to persist the transitions (DB
// updates, pub/sub notifications, etc.). The Agent guarantees these callbacks
// fire at the correct moments and in the correct order — application code never
// mutates turn state from other code paths.
//
// Each lifecycle callback has a contract: the implementation MUST persist the
// state transition AND publish a corresponding turn.updated event to the
// frontend. turn-agent does not emit pub/sub events directly — lifecycle
// callbacks are the sole integration point for frontend notifications.
//
// # What is NOT this package's responsibility
//
// turn-agent focuses exclusively on per-turn execution. The following concerns
// live OUTSIDE the package — the main application (typically via rtc-queue's
// Worker callbacks) handles them:
//
//   - Session lifecycle: updating session status (active/idle), publishing
//     session.updated events, managing the session's actor registry.
//
//   - Unhandled / late work items: re-enqueuing items that didn't get processed
//     due to cancellation or worker shutdown. rtc-queue tracks these at the
//     queue layer.
//
//   - Graceful preemption: the pkg supports graceful preemption via
//     Config.Cancel.GracePeriod. If set, cancellation waits up to the given
//     duration for the current model/tool call to finish at a safe point
//     before stopping. If zero (default), cancellation is immediate. This is
//     an agent-level setting — all cancel events for this agent use the same
//     mode. Applications that need different behavior per cancel event can
//     run separate agents with different configs or use rtc-queue's cancel
//     reason to drive higher-level policy.
//
//   - Turn merging: in the old in-process model (turn-loop), multiple queued
//     items could be consumed in a single iteration and merged into one turn.
//     In the distributed model, each Work item spawns its own turn. rtc-queue's
//     session-level lock serializes them; rapid user messages produce multiple
//     turns, each loading full history independently.
//
// If your application previously relied on turn-loop's Session object for any
// of these concerns, you must re-implement them at the application layer when
// migrating to turn-agent.
//
// # Integration with rtc-queue
//
// Agent.Process matches the signature of rtcqueue.WorkerConfig.OnWork, so it
// plugs directly into rtc-queue's Worker:
//
//	agent, err := turnagent.New(turnagent.Config{
//	    // Turn ownership — allocate / locate turns in the DB.
//	    CreateTurn: func(ctx context.Context, sessionID, workID string) (string, error) { ... },
//	    LookupTurn: func(ctx context.Context, sessionID, workID string) (string, error) { ... },
//
//	    // Lifecycle — persist state transitions, publish turn.updated events.
//	    BeginTurn:     func(ctx context.Context, turnID string) error { ... },
//	    CompleteTurn:  func(ctx context.Context, turnID string) error { ... },
//	    InterruptTurn: func(ctx context.Context, turnID string, interruptID string, info any) error { ... },
//	    ResumeTurn:    func(ctx context.Context, turnID string) error { ... },
//	    FailTurn:      func(ctx context.Context, turnID string, err error) error { ... },
//	    CancelTurn:    func(ctx context.Context, turnID string, reason string) error { ... },
//
//	    // Data — load/save messages, build tools/agent, publish events.
//	    LoadMessages: func(ctx context.Context, sessionID string) ([]*turnagent.Message, error) { ... },
//	    CreateTools:  func(ctx context.Context, sessionID, turnID string) ([]tool.BaseTool, error) { ... },
//	    CreateAgent:  func(ctx context.Context, sessionID, turnID string, tools []tool.BaseTool) (adk.Agent, error) { ... },
//	    PublishEvent: func(ctx context.Context, sessionID, turnID string, event *turnagent.Event) error { ... },
//
//	    CheckpointStore: redisStore,
//	})
//
//	worker := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
//	    WorkerID:    "worker-1",
//	    Concurrency: 4,
//	    OnWork:      agent.Process,
//	})
//	go worker.Run(ctx)
//
// Note that Session management is NOT in the Agent — the main application
// publishes bare Work items (kind + sessionID only) into rtc-queue. turn-agent
// owns the turn; the application owns the session.
//
// # Work item format
//
// rtc-queue Work items carry a JSON-encoded WorkPayload in their Data field.
// Two kinds exist:
//
//   - Submit: a new user message, starts a fresh turn.
//   - Resume: continues an interrupted turn from the eino checkpoint.
//
// rtc-queue's priority ordering guarantees that Resume items are always processed
// before Submit items within the same session, so the turn's lifecycle is strictly
// ordered and a resumed turn always sees its own checkpoint intact.
//
// # Observability
//
// turn-agent provides comprehensive observability through three optional interfaces:
// Logger, Tracer, and Metrics. All are nil by default and can be enabled independently.
// Observability failures never affect the main agent flow — calls are wrapped in
// nil-checks and panics in user implementations are recovered (see below).
//
// Logger provides structured logging with context support. All methods accept a
// context for extracting trace IDs and session information. The pkg emits logs at
// the following lifecycle points:
//
//   - agent.new         — when New() constructs an Agent (debug)
//   - turn.start        — after CreateTurn/LookupTurn, before BeginTurn/ResumeTurn (info)
//   - turn.panicked     — a user callback panicked; FailTurn was best-effort called (error)
//   - turn.begin_failed / turn.resume_failed — if a start callback returns an error (error)
//   - turn.end          — at every terminal state: success / interrupt / fail / cancel (info)
//   - turn.empty_messages — LoadMessages returned an empty slice (warn)
//   - turn.{complete,interrupt,fail}_callback_failed — a terminal callback returned an
//     error; the turn still transitioned (error)
//   - interrupt         — when eino raises a stateful interrupt (info)
//   - event.error       — when an event-level error is dispatched (error)
//
// Tracer provides OpenTelemetry distributed tracing. It uses the standard
// trace.Tracer interface and creates the following span hierarchy:
//
//   - Turn (one span per Process invocation): covers the entire turn, carries
//     session.id, turn.id, turn.work_kind, turn.status, turn.duration_ms.
//     Interrupt events appear as span events.
//
// The pkg does NOT wrap BuildInput / CreateAgent / etc. in their own spans —
// applications that want deeper hierarchy can wrap those callbacks themselves
// before handing them to Config.
//
// Metrics provides pluggable metrics collection for turns, LLM calls, interrupts,
// and checkpoints. Each method receives a struct with all relevant attributes:
//
//   - RecordTurn        — called exactly once per terminal turn outcome
//   - RecordInterrupt   — called when eino raises a stateful interrupt
//   - RecordLLMCall     — NOT called automatically; see the Metrics interface doc
//   - RecordCheckpoint  — NOT called automatically; see the Metrics interface doc
//
// # Panic recovery
//
// rtc-queue's Worker has no panic recovery around OnWork. turn-agent compensates
// by installing a deferred recover at the top of Process. Any panic in a user
// callback is converted into a FailTurn transition so the turn reaches a terminal
// state, and Process returns the recovered error so rtc-queue marks the work in
// "processing" for admin recovery. Panics inside observability callbacks (Logger,
// Tracer, Metrics) are also recovered locally so they cannot abort the turn.
//
// All observability features are optional. Set the corresponding field to nil
// to disable.
package turnagent
