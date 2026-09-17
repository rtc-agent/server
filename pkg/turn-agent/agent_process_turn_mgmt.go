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
	switch kind {
	case WorkKindSubmit:
		turnID, err := a.cfg.CreateTurn(ctx, sessionID, workID)
		if err != nil {
			return "", fmt.Errorf("CreateTurn: %w", err)
		}
		return turnID, nil
	case WorkKindResume:
		turnID, err := a.cfg.LookupTurn(ctx, sessionID, workID)
		if err != nil {
			return "", fmt.Errorf("LookupTurn: %w", err)
		}
		return turnID, nil
	default:
		return "", fmt.Errorf("unknown work kind: %q", kind)
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
		case cm := <-cancel:
			mgr.SetCancelledByQueue(cm.Reason)
			if a.cfg.Cancel.GracePeriod > 0 {
				mgr.Loop().Stop(adk.WithGracefulTimeout(a.cfg.Cancel.GracePeriod))
			} else {
				mgr.Loop().Stop(adk.WithImmediate())
			}
			innerCancel()
			turnCancel()
		case <-done:
			return
		}
	}()
}
