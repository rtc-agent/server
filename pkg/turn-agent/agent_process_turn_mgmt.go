package turnagent

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/cloudwego/eino/adk"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// resolveTurnID maps a (sessionID, workID, kind) tuple to a turn ID.
//
// For WorkKindSubmit it creates a new turn; for WorkKindResume it looks up the
// existing turn. Returns ErrNoActiveTurn (unwrapped) when a resume target is
// no longer active, so callers can distinguish transient from permanent errors.
func (a *Agent) resolveTurnID(ctx context.Context, sessionID, workID string, kind WorkKind) (string, error) {
	resolveCtx, resolveSpan := a.startSpanIfEnabled(ctx, "resolve_turn_id",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("work.id", workID),
			attribute.String("work.kind", string(kind)),
		),
	)
	defer resolveSpan.End()

	var turnID string
	var err error

	switch kind {
	case WorkKindSubmit:
		resolveSpan.AddEvent("create_turn")
		turnID, err = a.cfg.CreateTurn(resolveCtx, sessionID, workID)
		if err != nil {
			resolveSpan.RecordError(err)
			resolveSpan.SetAttributes(attribute.String("turn.status", "error"))
			a.log(resolveCtx, LogLevelError, "resolve_turn_id.create_failed", map[string]any{
				"session_id": sessionID,
				"work_id":    workID,
				"error":      err.Error(),
			})
			return "", fmt.Errorf("CreateTurn: %w", err)
		}
		resolveSpan.SetAttributes(attribute.String("turn.id", turnID))
		a.log(resolveCtx, LogLevelInfo, "resolve_turn_id.created", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"work_id":    workID,
		})
		return turnID, nil
	case WorkKindResume:
		resolveSpan.AddEvent("lookup_turn")
		turnID, err = a.cfg.LookupTurn(resolveCtx, sessionID, workID)
		if err != nil {
			resolveSpan.RecordError(err)
			resolveSpan.SetAttributes(attribute.String("turn.status", "error"))
			a.log(resolveCtx, LogLevelError, "resolve_turn_id.lookup_failed", map[string]any{
				"session_id": sessionID,
				"work_id":    workID,
				"error":      err.Error(),
			})
			return "", fmt.Errorf("LookupTurn: %w", err)
		}
		resolveSpan.SetAttributes(attribute.String("turn.id", turnID))
		a.log(resolveCtx, LogLevelInfo, "resolve_turn_id.resumed", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"work_id":    workID,
		})
		return turnID, nil
	default:
		err := fmt.Errorf("unknown work kind: %q", kind)
		resolveSpan.RecordError(err)
		return "", err
	}
}

// beginTurn invokes the appropriate Begin/Resume callback when the caller is
// the owner of a new session manager (isNew=true). It records a tracing
// attribute and an error log on failure so the caller can simply propagate the
// returned error.
func (a *Agent) beginTurn(ctx context.Context, span trace.Span, sessionID, turnID string, kind WorkKind) error {
	switch kind {
	case WorkKindSubmit:
		if err := a.cfg.BeginTurn(ctx, turnID); err != nil {
			span.SetAttributes(attribute.String("turn.status", "error"))
			a.log(ctx, LogLevelError, "turn.begin_failed", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: BeginTurn: %w", err)
		}
	case WorkKindResume:
		if err := a.cfg.ResumeTurn(ctx, turnID); err != nil {
			span.SetAttributes(attribute.String("turn.status", "error"))
			a.log(ctx, LogLevelError, "turn.resume_failed", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: ResumeTurn: %w", err)
		}
	default:
		return fmt.Errorf("turnagent: beginTurn: unknown work kind: %q", kind)
	}
	return nil
}

// startCancelListener runs a goroutine that watches the queue's cancel channel
// and, on cancellation, stops the session turn loop and propagates the
// cancellation to both the inner work context and the turn-level context (which
// also cancels the in-flight LLM call running in mgr.Run).
func (a *Agent) startCancelListener(
	cancel <-chan rtcqueue.CancelMessage,
	mgr *SessionTurnManager,
	done <-chan struct{},
	innerCancel context.CancelFunc,
	turnCancel context.CancelFunc,
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				zap.L().Error("agent_process.cancel_listener_panic",
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())),
				)
			}
		}()
		select {
		case cm, ok := <-cancel:
			if !ok {
				// cancel channel closed without delivering a message.
				// This means the worker's cancel subscription ended (e.g. workCtx
				// cancelled or Pub/Sub connection lost) before a cancel arrived.
				a.log(context.Background(), LogLevelWarn, "cancel_listener.channel_closed", map[string]any{
					"session_id": mgr.SessionID(),
					"turn_id":    mgr.TurnID(),
					"message":    "cancel channel closed without cancel message; worker may have lost subscription",
				})
				return
			}
			a.log(context.Background(), LogLevelInfo, "cancel_listener.received", map[string]any{
				"session_id": mgr.SessionID(),
				"turn_id":    mgr.TurnID(),
				"work_id":    cm.WorkID,
				"reason":     cm.Reason,
			})
			mgr.SetCancelledByQueue(cm.Reason)
			if a.cfg.Cancel.GracePeriod > 0 {
				a.log(context.Background(), LogLevelInfo, "cancel_listener.stop_graceful", map[string]any{
					"session_id":   mgr.SessionID(),
					"turn_id":      mgr.TurnID(),
					"grace_period": a.cfg.Cancel.GracePeriod.String(),
				})
				mgr.Loop().Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				a.log(context.Background(), LogLevelInfo, "cancel_listener.stop_immediate", map[string]any{
					"session_id": mgr.SessionID(),
					"turn_id":    mgr.TurnID(),
				})
				mgr.Loop().Stop(adk.WithImmediate())
			}
			innerCancel()
			turnCancel()
			a.log(context.Background(), LogLevelInfo, "cancel_listener.cancel_sent", map[string]any{
				"session_id": mgr.SessionID(),
				"turn_id":    mgr.TurnID(),
			})
		case <-done:
			a.log(context.Background(), LogLevelDebug, "cancel_listener.done_closed", map[string]any{
				"session_id":   mgr.SessionID(),
				"turn_id":      mgr.TurnID(),
				"is_cancelled": mgr.IsCancelledByQueue(),
				"message":      "Process() returned; cancel listener exiting",
			})
			return
		}
	}()
}
