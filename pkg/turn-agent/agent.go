package turnagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// Agent is a stateless processor that executes one turn per rtc-queue Work
// item. All persistent state lives in the eino checkpoint store and the
// application's database.
//
// An Agent is safe to share across goroutines and may be used as the OnWork
// callback of one or more rtcqueue.Worker instances.
type Agent struct {
	cfg Config
}

// New constructs an Agent. Returns an error if required Config fields are
// missing.
func New(cfg Config) (*Agent, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("turnagent: %w", err)
	}
	if cfg.DeriveCheckpointID == nil {
		cfg.DeriveCheckpointID = func(sessionID string) string {
			return "turnagent:session:" + sessionID
		}
	}
	a := &Agent{cfg: cfg}
	a.logIfEnabled(context.Background(), LogLevelDebug, "agent.new", map[string]any{
		"has_logger":         cfg.Logger != nil,
		"has_tracer":         cfg.Tracer != nil,
		"has_metrics":        cfg.Metrics != nil,
		"enable_llm_logging": cfg.EnableLLMLogging,
		"has_callbacks":      len(cfg.Callbacks) > 0,
	})
	return a, nil
}

// Process executes one turn for the given rtc-queue Work item. Its signature
// matches rtcqueue.WorkerConfig.OnWork, so it plugs directly into rtc-queue's
// Worker.
//
// Process owns the turn's lifecycle. It creates / looks up the turn via
// Config callbacks, drives the eino TurnLoop, and calls the lifecycle
// callbacks (Begin/Resume/Complete/Interrupt/Fail/Cancel Turn) at the
// well-defined moments. Application code must NOT mutate turn state from
// other code paths.
//
// Process returns nil when the turn has reached a terminal state through the
// appropriate callback (Complete / Interrupt / Cancel). It returns a non-nil
// error when:
//   - The work payload could not be decoded
//   - CreateTurn / LookupTurn failed — the work is left in "processing"
//     status for admin recovery
//   - A start callback (BeginTurn / ResumeTurn) failed — same outcome
//   - The turn ended due to an unexpected error — FailTurn has been called,
//     and the error is propagated so rtc-queue keeps the work in "processing"
//   - The parent ctx was cancelled (graceful worker shutdown) — no lifecycle
//     callback is invoked, so the turn stays in its previous state and can be
//     picked up by another worker once the session lock expires
func (a *Agent) Process(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) (retErr error) {
	// 1. Decode payload — just {kind, sessionID}.
	var p WorkPayload
	if err := json.Unmarshal([]byte(work.Data), &p); err != nil {
		return fmt.Errorf("turnagent: decode work payload: %w", err)
	}
	if p.SessionID == "" {
		return fmt.Errorf("turnagent: work payload missing session_id")
	}
	a.logIfEnabled(ctx, LogLevelInfo, "agent.process", map[string]any{
		"p.SessionID": p.SessionID,
		"p.Kind":      p.Kind,
	})

	// 2. Obtain the turnID.
	//
	// For submit: CreateTurn allocates a new turn (e.g., UUID + DB row).
	// For resume: LookupTurn finds the existing active turn for this session.
	//
	// turnID is then threaded through every subsequent callback for the
	// duration of this work — including across the eino Run(), so closures
	// in buildEinoConfig capture it.
	var (
		turnID string
		err    error
	)
	switch p.Kind {
	case WorkKindSubmit:
		// Pass work.ID as an idempotency key so CreateTurn can upsert on
		// (sessionID, workID) and tolerate rtc-queue retries.
		turnID, err = a.cfg.CreateTurn(ctx, p.SessionID, work.ID)
		if err != nil {
			return fmt.Errorf("turnagent: CreateTurn: %w", err)
		}
	case WorkKindResume:
		// workID is reserved for future idempotent resume. Implementations
		// may ignore it; the signature matches CreateTurn for symmetry.
		turnID, err = a.cfg.LookupTurn(ctx, p.SessionID, work.ID)
		if err != nil {
			return fmt.Errorf("turnagent: LookupTurn: %w", err)
		}
	default:
		return fmt.Errorf("turnagent: unknown work kind: %q", p.Kind)
	}

	// 2.5. Observability: start a turn span covering the entire Process
	// invocation from this point forward.
	turnCtx, turnSpan := a.startSpanIfEnabled(ctx, "turn")
	defer turnSpan.End()
	turnSpan.SetAttributes(
		attribute.String("session.id", p.SessionID),
		attribute.String("turn.id", turnID),
		attribute.String("turn.work_kind", string(p.Kind)),
	)
	turnStart := time.Now()
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.start", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
	})

	// 2.6. Panic recovery. rtc-queue's Worker does not recover panics from
	// OnWork, so the pkg must guard Process itself. Any panic in a user
	// callback is converted into a FailTurn transition so the turn reaches
	// a terminal state, and Process returns the recovered error so rtc-queue
	// marks the work in "processing" for admin recovery.
	//
	// The recovery is placed AFTER turnID is known so FailTurn can be called
	// with a valid ID. If the panic occurs during CreateTurn/LookupTurn
	// itself (before turnID exists), there is no turn to transition — the
	// recovered panic is returned as a raw error.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		var pErr error
		switch v := r.(type) {
		case error:
			pErr = fmt.Errorf("turnagent: panic in callback: %w", v)
		default:
			pErr = fmt.Errorf("turnagent: panic in callback: %v", v)
		}
		a.logIfEnabled(turnCtx, LogLevelError, "turn.panicked", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"error":      pErr.Error(),
		})
		// Best-effort transition to failed. Guard with an inner recover so a
		// misbehaving FailTurn does not swallow the original panic.
		if turnID != "" {
			func() {
				defer func() { _ = recover() }()
				_ = a.cfg.FailTurn(turnCtx, turnID, pErr)
			}()
		}
		retErr = pErr
	}()

	// 3. Build eino config and TurnLoop.
	//
	// buildEinoConfig captures sessionID and turnID in closures, so
	// GenInput / GenResume / PrepareAgent / OnAgentEvents can pass them to
	// the application's data callbacks without needing them in the eino
	// item type.
	checkpointID := a.cfg.DeriveCheckpointID(p.SessionID)
	einoCfg := a.buildEinoConfig(p.SessionID, turnID, checkpointID)
	loop := adk.NewTurnLoop[WorkPayload, *schema.Message](einoCfg)

	// 4. Cancel listener.
	//
	// An inner context is used so we can cancel eino's Run without affecting
	// the parent ctx (which the rtc-queue Worker owns). cancelledByQueue
	// disambiguates "explicit admin cancel" from "parent ctx cancelled by
	// worker shutdown" — only the former should transition the turn to
	// "cancelled".
	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var cancelledByQueue atomic.Bool
	var cancelReason string
	go func() {
		select {
		case cm := <-cancel:
			cancelReason = cm.Reason
			cancelledByQueue.Store(true)
			innerCancel()
			// Build stop options based on Cancel config.
			//
			// GracePeriod > 0 → graceful: wait up to GracePeriod for the
			// current model/tool call to finish at a safe point, then stop.
			// This avoids half-generated responses visible to the frontend.
			//
			// GracePeriod == 0 (default) → immediate: abort as soon as
			// possible. Use this for admin cancels where responsiveness
			// matters more than output coherence.
			if a.cfg.Cancel.GracePeriod > 0 {
				loop.Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				loop.Stop(adk.WithImmediate())
			}
		case <-innerCtx.Done():
			return
		}
	}()

	// 5. Turn lifecycle: start.
	switch p.Kind {
	case WorkKindSubmit:
		if err := a.cfg.BeginTurn(turnCtx, turnID); err != nil {
			turnSpan.SetAttributes(attribute.String("turn.status", "error"))
			a.logIfEnabled(turnCtx, LogLevelError, "turn.begin_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: BeginTurn: %w", err)
		}
	case WorkKindResume:
		if err := a.cfg.ResumeTurn(turnCtx, turnID); err != nil {
			turnSpan.SetAttributes(attribute.String("turn.status", "error"))
			a.logIfEnabled(turnCtx, LogLevelError, "turn.resume_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: ResumeTurn: %w", err)
		}
	}

	// 6. Push payload into eino's buffer and run.
	//
	// The payload itself carries only {kind, sessionID}; eino uses it only
	// as a trigger. Actual per-turn data flows through the closures in
	// buildEinoConfig (which have sessionID/turnID) and the data callbacks
	// (LoadMessages etc.).
	//
	// For Submit: eino calls GenInput (no checkpoint exists).
	// For Resume: eino finds the checkpoint, calls GenResume instead.
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.pushing_to_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
	})
	pushed, pushAck := loop.Push(p)
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.push_result", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
		"work_kind":  string(p.Kind),
		"pushed":     pushed,
		"has_ack":    pushAck != nil,
	})
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.running_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
	})
	loop.Run(turnCtx)
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.waiting_loop", map[string]any{
		"session_id": p.SessionID,
		"turn_id":    turnID,
	})
	exit := loop.Wait()

	// Debug: log eino exit reason to trace why turn didn't complete
	a.logIfEnabled(turnCtx, LogLevelInfo, "turn.eino_exited", map[string]any{
		"session_id":  p.SessionID,
		"turn_id":     turnID,
		"work_kind":   string(p.Kind),
		"exit_reason": fmt.Sprintf("%v", exit.ExitReason),
		"has_error":   exit.ExitReason != nil,
	})

	// 7. Turn lifecycle: end.
	//
	// Priority order matters: cancelledByQueue takes precedence over
	// exit.ExitReason, because an admin cancel may surface as a generic
	// context.Canceled or an eino CancelError.
	//
	// Observability: compute duration and status once, then emit to
	// Logger / Tracer / Metrics before invoking the terminal callback.
	turnDuration := time.Since(turnStart)

	recordEnd := func(status string, err error) {
		// Guard against panics from Logger / Tracer / Metrics implementations.
		// Observability must never break the turn's terminal transition.
		defer func() { _ = recover() }()
		turnSpan.SetAttributes(
			attribute.String("turn.status", status),
			attribute.Int64("turn.duration_ms", turnDuration.Milliseconds()),
		)
		if err != nil {
			turnSpan.RecordError(err)
		}
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.end", map[string]any{
			"session_id":  p.SessionID,
			"turn_id":     turnID,
			"work_kind":   string(p.Kind),
			"status":      status,
			"duration_ms": turnDuration.Milliseconds(),
			"error":       fmt.Sprintf("%v", err),
		})
		a.recordMetricIfEnabled(turnCtx, func(m Metrics) {
			m.RecordTurn(turnCtx, TurnMetricsAttrs{
				SessionID:  p.SessionID,
				TurnID:     turnID,
				WorkKind:   string(p.Kind),
				Status:     status,
				DurationMs: turnDuration.Milliseconds(),
				Error:      err,
			})
		})
	}

	if cancelledByQueue.Load() {
		recordEnd("cancel", nil)
		_ = a.cfg.CancelTurn(turnCtx, turnID, cancelReason)
		return nil
	}

	if errors.Is(innerCtx.Err(), context.Canceled) && ctx.Err() != nil {
		// Parent ctx cancelled (worker shutting down). Don't transition the
		// turn — leave it in its previous state so another worker can pick
		// up the work when the session lock expires. No terminal observability
		// emission: the turn didn't reach a terminal state.
		return ctx.Err()
	}

	switch {
	case exit.ExitReason == nil:
		// Clean exit. eino has deleted the checkpoint.
		a.logIfEnabled(turnCtx, LogLevelInfo, "turn.clean_exit", map[string]any{
			"session_id": p.SessionID,
			"turn_id":    turnID,
			"message":    "calling CompleteTurn",
		})
		recordEnd("success", nil)
		if err := a.cfg.CompleteTurn(turnCtx, turnID); err != nil {
			// Terminal callback error: log it, but the turn has reached a
			// terminal state from eino's perspective. Return nil so rtc-queue
			// marks the work as complete; the DB inconsistency is for admin
			// reconciliation, not a reason to retry the turn.
			a.logIfEnabled(turnCtx, LogLevelError, "turn.complete_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		} else {
			a.logIfEnabled(turnCtx, LogLevelInfo, "turn.completed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
			})
		}
		return nil

	case isInterruptError(exit.ExitReason):
		// Interrupt is a legitimate turn pause, not an error.
		// eino has persisted the checkpoint.
		var iErr *adk.InterruptError
		_ = errors.As(exit.ExitReason, &iErr)
		root := rootInterruptCtx(iErr.InterruptContexts)
		if root == nil {
			// Defensive: shouldn't happen for a well-formed InterruptError,
			// but guard anyway so we don't panic below.
			recordEnd("fail", exit.ExitReason)
			if err := a.cfg.FailTurn(turnCtx, turnID, exit.ExitReason); err != nil {
				a.logIfEnabled(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
					"session_id": p.SessionID,
					"turn_id":    turnID,
					"error":      err.Error(),
				})
			}
			// The turn has reached a terminal state from eino's perspective;
			// return the underlying error so rtc-queue leaves the work in
			// "processing" for admin recovery.
			return exit.ExitReason
		}
		// Observability: interrupt event.
		a.logIfEnabled(turnCtx, LogLevelInfo, "interrupt", map[string]any{
			"session_id":   p.SessionID,
			"turn_id":      turnID,
			"interrupt_id": root.ID,
			"reason":       "stateful_interrupt",
		})
		a.addEventIfEnabled(turnCtx, "interrupt",
			attribute.String("session.id", p.SessionID),
			attribute.String("turn.id", turnID),
			attribute.String("interrupt.id", root.ID),
			attribute.String("reason", "stateful_interrupt"),
		)
		a.recordMetricIfEnabled(turnCtx, func(m Metrics) {
			m.RecordInterrupt(turnCtx, InterruptMetricsAttrs{
				SessionID:   p.SessionID,
				TurnID:      turnID,
				InterruptID: root.ID,
				Reason:      "stateful_interrupt",
			})
		})
		recordEnd("interrupt", nil)
		if err := a.cfg.InterruptTurn(turnCtx, turnID, root.ID, root.Info); err != nil {
			// Terminal callback error: log it, but the turn has paused.
			// Return nil so rtc-queue marks the work as complete; the DB
			// inconsistency is for admin reconciliation.
			a.logIfEnabled(turnCtx, LogLevelError, "turn.interrupt_callback_failed", map[string]any{
				"session_id":   p.SessionID,
				"turn_id":      turnID,
				"interrupt_id": root.ID,
				"error":        err.Error(),
			})
		}
		return nil

	default:
		// Unexpected error.
		recordEnd("fail", exit.ExitReason)
		if err := a.cfg.FailTurn(turnCtx, turnID, exit.ExitReason); err != nil {
			a.logIfEnabled(turnCtx, LogLevelError, "turn.fail_callback_failed", map[string]any{
				"session_id": p.SessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
		}
		// The turn has reached a terminal state from eino's perspective;
		// return the underlying error so rtc-queue leaves the work in
		// "processing" for admin recovery.
		return exit.ExitReason
	}
}

// buildEinoConfig constructs the eino TurnLoopConfig from the application's
// callbacks. sessionID and turnID are captured in closures so GenInput,
// GenResume, PrepareAgent, and OnAgentEvents can forward them to the data
// callbacks without carrying them in the eino item type.
func (a *Agent) buildEinoConfig(sessionID, turnID, checkpointID string) adk.TurnLoopConfig[WorkPayload, *schema.Message] {
	return adk.TurnLoopConfig[WorkPayload, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[WorkPayload, *schema.Message], items []WorkPayload) (*adk.GenInputResult[WorkPayload, *schema.Message], error) {
			// Inject eino callbacks into ctx so downstream model/tool calls
			// pick up application-level tracing / metrics handlers. The empty
			// RunInfo mirrors turn-loop's session-level injection — per-component
			// RunInfo is filled in by eino as it walks the agent tree.
			if len(a.cfg.Callbacks) > 0 {
				ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, a.cfg.Callbacks...)
			}

			msgs, err := a.cfg.LoadMessages(ctx, sessionID)
			if err != nil {
				return nil, fmt.Errorf("turnagent: LoadMessages: %w", err)
			}
			if len(msgs) == 0 {
				// The implementation is responsible for always returning a
				// meaningful history (system prompt + user message at minimum).
				// An empty slice almost always indicates a bug upstream —
				// log it so the issue surfaces in logs, but still run the
				// agent with empty input so we don't mask the underlying
				// state by failing the turn.
				a.logIfEnabled(ctx, LogLevelWarn, "turn.empty_messages", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
				})
			}
			return &adk.GenInputResult[WorkPayload, *schema.Message]{
				RunCtx: ctx,
				Input: &adk.TypedAgentInput[*schema.Message]{
					Messages:        toEinoMessages(msgs),
					EnableStreaming: true,
				},
				Consumed: items,
			}, nil
		},

		GenResume: func(ctx context.Context, loop *adk.TurnLoop[WorkPayload, *schema.Message], interrupted, unhandled, newItems []WorkPayload) (*adk.GenResumeResult[WorkPayload, *schema.Message], error) {
			// RTC 模式下：不需要向 ResumeParams 里塞任何数据。工具在重新进入后
			// 通过 tool.GetInterruptState 拿到 RtcID，自己查 DB 取结果。
			//
			// 若未来引入非 RTC 的中断类型（例如简单的确认类问答）且需要把 answer
			// 透传给工具，可在此处添加一个 LoadResumeData 回调，把结果塞到
			// ResumeParams.Targets。
			return &adk.GenResumeResult[WorkPayload, *schema.Message]{
				RunCtx: ctx,
				// ResumeParams 留空。
			}, nil
		},

		PrepareAgent: func(ctx context.Context, loop *adk.TurnLoop[WorkPayload, *schema.Message], consumed []WorkPayload) (adk.Agent, error) {
			// Pass turnID so the implementation can inject it into tool /
			// agent contexts (for tracing, DB writes, RTC state lookup, etc.).
			tools, err := a.cfg.CreateTools(ctx, sessionID, turnID)
			if err != nil {
				return nil, fmt.Errorf("turnagent: CreateTools: %w", err)
			}
			agent, err := a.cfg.CreateAgent(ctx, sessionID, turnID, tools)
			if err != nil {
				return nil, fmt.Errorf("turnagent: CreateAgent: %w", err)
			}
			return agent, nil
		},

		OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[WorkPayload, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				ev, ok := events.Next()
				if !ok {
					// Agent 已完成事件生产，通知 loop 在当前 turn 结束后退出。
					// Stop() 无参数：让当前 turn 完成，但不开始新 turn。
					// 这解决了 loop 在成功后永久阻塞等待更多 item 的问题。
					tc.Loop.Stop()
					return nil
				}
				if err := a.dispatchEvents(ctx, sessionID, turnID, ev); err != nil {
					tc.Loop.Stop()
					return fmt.Errorf("turnagent: PublishEvent: %w", err)
				}
			}
		},

		Store:        a.cfg.CheckpointStore,
		CheckpointID: checkpointID,
	}
}

// dispatchEvents translates one eino AgentEvent into one or more flattened
// pkg Event calls to the application's PublishEvent callback.
//
// For streaming events, the pkg fully consumes the underlying stream, emitting
// EventKindStreamChunk per Recv() and EventKindStreamEnd at EOF. The upper
// application never touches the stream object itself.
//
// The stream consumption loop respects ctx cancellation: if ctx is cancelled
// while Recv() is blocking, the stream is closed and the loop exits cleanly.
//
// CancelError is intentionally swallowed: it is eino's internal cancellation
// signal, not an application-visible error. The turn's cancellation is already
// handled by the lifecycle path in Process() (via the cancel channel and the
// cancelledByQueue flag).
func (a *Agent) dispatchEvents(ctx context.Context, sessionID, turnID string, ev *adk.AgentEvent) error {
	// 1. Event-level error.
	if ev.Err != nil {
		// Swallow CancelError — it's eino's internal cancellation signal.
		// The turn-level cancel path in Process() handles the lifecycle
		// transition; propagating CancelError here would cause eino's
		// TurnLoop to terminate unexpectedly.
		var cancelErr *adk.CancelError
		if errors.As(ev.Err, &cancelErr) {
			return nil
		}
		a.logIfEnabled(ctx, LogLevelError, "event.error", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"agent_name": ev.AgentName,
			"error":      ev.Err.Error(),
		})
		return a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
			Kind:      EventKindError,
			AgentName: ev.AgentName,
			Err:       ev.Err,
		})
	}

	// 2. No output — skip.
	if ev.Output == nil || ev.Output.MessageOutput == nil {
		return nil
	}
	mv := ev.Output.MessageOutput

	// 3. Streaming: consume the stream, emit chunks + end.
	if mv.IsStreaming {
		return a.consumeStream(ctx, sessionID, turnID, ev.AgentName, string(mv.Role), mv.ToolName, mv.MessageStream)
	}

	// 4. Non-streaming: emit one message event.
	return a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
		Kind:      EventKindMessage,
		AgentName: ev.AgentName,
		Role:      string(mv.Role),
		ToolName:  mv.ToolName,
		Message:   fromEinoMessage(mv.Message),
	})
}

// consumeStream drives a stream reader to completion, translating each
// received chunk into an EventKindStreamChunk callback and emitting a final
// EventKindStreamEnd on EOF.
//
// The loop uses a goroutine + select pattern so that ctx cancellation unblocks
// stream.Recv() — closing the stream on the way out so eino's resources are
// released.
func (a *Agent) consumeStream(ctx context.Context, sessionID, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message]) error {
	// No `defer stream.Close()` here: we close explicitly on each exit path
	// below. A deferred close would fire on top of the explicit close on the
	// ctx.Done / EOF / error paths, causing a double close.

	a.logIfEnabled(ctx, LogLevelDebug, "stream.consume_start", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
		"agent_name": agentName,
		"role":       role,
	})

	type recvResult struct {
		msg *schema.Message
		err error
	}

	for {
		// Recv in a goroutine so we can race it against ctx cancellation.
		ch := make(chan recvResult, 1)
		go func() {
			msg, err := stream.Recv()
			ch <- recvResult{msg, err}
		}()

		select {
		case <-ctx.Done():
			// ctx cancelled (either admin cancel or worker shutdown). Close the
			// stream and propagate. No EventKindStreamEnd is emitted — the turn
			// is being torn down via the cancel/fail path in Process().
			a.logIfEnabled(ctx, LogLevelInfo, "stream.ctx_cancelled", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
			})
			stream.Close()
			return ctx.Err()

		case res := <-ch:
			if errors.Is(res.err, io.EOF) {
				// Stream complete. Close the underlying reader before emitting
				// the terminal event, so eino releases its resources before
				// the application processes the end marker.
				a.logIfEnabled(ctx, LogLevelDebug, "stream.eof", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
				})
				stream.Close()
				return a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
					Kind:      EventKindStreamEnd,
					AgentName: agentName,
					Role:      role,
					ToolName:  toolName,
				})
			}
			if res.err != nil {
				// Swallow CancelError — same reason as in dispatchEvents.
				var cancelErr *adk.CancelError
				if errors.As(res.err, &cancelErr) {
					stream.Close()
					return nil
				}
				// Transport error mid-stream. Surface as an event error; the
				// turn's final disposition is decided by the caller based on
				// how the TurnLoop exits.
				stream.Close()
				if pubErr := a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
					Kind:      EventKindError,
					AgentName: agentName,
					Err:       res.err,
				}); pubErr != nil {
					return pubErr
				}
				return res.err
			}

			// Valid chunk. Translate and forward.
			var finishReason string
			if res.msg.ResponseMeta != nil {
				finishReason = res.msg.ResponseMeta.FinishReason
			}
			a.logIfEnabled(ctx, LogLevelDebug, "stream.chunk", map[string]any{
				"session_id":    sessionID,
				"turn_id":       turnID,
				"agent_name":    agentName,
				"role":          role,
				"finish_reason": finishReason,
				"has_content":   res.msg.Content != "",
				"is_final":      finishReason != "",
			})
			if err := a.cfg.PublishEvent(ctx, sessionID, turnID, &Event{
				Kind:             EventKindStreamChunk,
				AgentName:        agentName,
				Role:             role,
				ToolName:         toolName,
				Content:          res.msg.Content,
				ReasoningContent: res.msg.ReasoningContent,
				FinishReason:     finishReason,
			}); err != nil {
				// PublishEvent error: close the stream before propagating so
				// eino resources are released.
				stream.Close()
				return err
			}
		}
	}
}

// =============================================================================
// Helpers
// =============================================================================

func isInterruptError(err error) bool {
	var iErr *adk.InterruptError
	return errors.As(err, &iErr)
}

// rootInterruptCtx returns the root interrupt context — the deepest tool that
// actually raised the interrupt. Falls back to the first context if no root
// cause is marked. Returns nil if the slice is empty.
func rootInterruptCtx(ctxs []*adk.InterruptCtx) *adk.InterruptCtx {
	if len(ctxs) == 0 {
		return nil
	}
	for _, c := range ctxs {
		if c.IsRootCause {
			return c
		}
	}
	return ctxs[0]
}

// =============================================================================
// Observability helpers
// =============================================================================

// cachedNoopSpan is a package-level noop span used when Tracer is nil.
// Using a noop trace span (rather than trace.SpanFromContext(ctx)) prevents
// unconditional SetAttributes/End calls from polluting an unrelated parent
// span that might already exist in the context.
var cachedNoopSpan trace.Span

func init() {
	_, cachedNoopSpan = nooptrace.NewTracerProvider().Tracer("turnagent").Start(context.Background(), "noop")
}

// logIfEnabled calls the logger if it is not nil.
func (a *Agent) logIfEnabled(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
	if a.cfg.Logger != nil {
		switch level {
		case LogLevelDebug:
			a.cfg.Logger.Debug(ctx, msg, fields)
		case LogLevelInfo:
			a.cfg.Logger.Info(ctx, msg, fields)
		case LogLevelWarn:
			a.cfg.Logger.Warn(ctx, msg, fields)
		case LogLevelError:
			a.cfg.Logger.Error(ctx, msg, fields)
		}
	}
}

// recordMetricIfEnabled calls the metrics recorder if it is not nil.
func (a *Agent) recordMetricIfEnabled(ctx context.Context, record func(Metrics)) {
	if a.cfg.Metrics != nil {
		record(a.cfg.Metrics)
	}
}

// startSpanIfEnabled starts a span if tracer is not nil.
// When tracing is disabled, returns a cached noop span so callers can invoke
// SetAttributes/End unconditionally without polluting an unrelated parent span.
func (a *Agent) startSpanIfEnabled(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if a.cfg.Tracer != nil {
		return a.cfg.Tracer.Start(ctx, name, opts...)
	}
	return ctx, cachedNoopSpan
}

// addEventIfEnabled adds an event to the current span if tracer is not nil.
func (a *Agent) addEventIfEnabled(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if a.cfg.Tracer != nil {
		span := trace.SpanFromContext(ctx)
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}
