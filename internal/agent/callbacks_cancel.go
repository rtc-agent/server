package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// cancelTurn marks a turn as "cancelled".
//
// Called when the turn is cancelled via rtc-queue's Cancel admin operation.
// reason carries the CancelMessage.Reason from the publisher.
func (h *helpers) cancelTurn(ctx context.Context, turnID string, reason string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("cancelTurn: invalid turn ID %q: %w", turnID, err)
	}

	// Use context.WithoutCancel + timeout for the critical status update.
	// The caller's ctx may already be cancelled (e.g., during worker shutdown).
	// Without isolation, the turn would be left in "running" state permanently,
	// requiring the stale turn scanner (5-30 minutes) to recover it.
	statusCtx, statusCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer statusCancel()

	if err := h.deps.TurnRepo.UpdateStatus(statusCtx, tid, protocol.TurnStatusCancelled, reason); err != nil {
		return fmt.Errorf("cancelTurn: update status: %w", err)
	}

	// IMPORTANT: do NOT reassign ctx = statusCtx here. The isolated context has
	// a 10s deadline shared with the critical UpdateStatus above. Subsequent
	// operations (GetByID, session UpdateStatus, Publish, notifyParent,
	// cascadeCancel) would share that shrinking budget. If UpdateStatus took 8s,
	// only 2s would remain for all remaining work.
	// Using the original ctx gives each operation its own full deadline.

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		// Fallback: extract sessionID from context and perform cleanup.
		// Delegates to shared helper (also used by failTurn) to eliminate
		// the 35-line duplicate that previously lived here.
		h.fallbackTurnLookupCleanup(ctx, turnID, tid, "cancel", "cancelled", &reason, "cancelTurn")
		return nil
	}

	// Set session status to "idle" — turn was cancelled.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logger.Warn(ctx, "cancelTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "cancel")

	h.logger.Info(ctx, "cancelTurn.done", map[string]any{
		"turn_id": turnID,
		"reason":  reason,
	})

	// Sub Agent support: if this is a sub session, notify the parent.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, turn.SessionID)
	if sessErr != nil {
		h.logger.Warn(ctx, "cancelTurn.load_session_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      sessErr.Error(),
		})
	}
	if session == nil && sessErr == nil {
		h.logger.Warn(ctx, "cancelTurn.session_nil", map[string]any{
			"session_id": turn.SessionID.String(),
			"turn_id":    turnID,
			"message":    "session not found; parent notification skipped",
		})
	}
	h.notifyParentAfterSubAgentSession(ctx, session, nil, "cancelled", &reason)

	// Cascade cancel: if this session has active child sessions (sub agents),
	// cancel them as well. This prevents orphaned sub agents from continuing
	// to run after the parent has been cancelled.
	h.cascadeCancelChildren(ctx, turnID, turn.SessionID)

	return nil
}

// cascadeCancelChildren propagates cancellation to all active child sessions
// (sub agents) of the given parent session. This prevents orphaned sub agents
// from continuing to run after the parent has been cancelled.
//
// The method detaches from the caller's context (which may already be cancelled
// when called from cancelTurn/failTurn) using context.WithoutCancel with a 30s
// timeout, ensuring child sessions are always cancelled even if the parent's
// context is done. Mirrors notifyParentAfterAsyncSubAgent's fire-and-forget pattern.
//
// The method is safe to call even when h.queue is nil or no children exist.
func (h *helpers) cascadeCancelChildren(callerCtx context.Context, turnID string, parentSessionID uuid.UUID) {
	if h.queue == nil {
		return
	}
	// Detach from the caller's context — it may already be cancelled (e.g., when
	// called from cancelTurn after the turn context was cancelled). Without this,
	// CancelSession calls would fail silently, leaving orphaned child sessions.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()
	activeChildren, findErr := h.deps.SessionRepo.FindActiveByParent(ctx, parentSessionID)
	if findErr != nil {
		h.logger.Warn(ctx, "cascadeCancelChildren.find_active_by_parent_failed", map[string]any{
			"turn_id":           turnID,
			"parent_session_id": parentSessionID.String(),
			"error":             findErr.Error(),
		})
		return
	}
	if len(activeChildren) == 0 {
		return
	}
	h.logger.Info(ctx, "cascadeCancelChildren.start", map[string]any{
		"turn_id":     turnID,
		"session_id":  parentSessionID.String(),
		"child_count": len(activeChildren),
	})
	// Cancel children in parallel so a slow CancelSession call does not
	// block the remaining children. Each child gets its own 30s timeout
	// (detached from the caller's context via WithoutCancel) so that a slow
	// cancel of one child does not cause the remaining children to hit the
	// shared deadline. Without per-child timeouts, 10 slow children could
	// exhaust the parent timeout before the later goroutines even start.
	var wg sync.WaitGroup
	for _, child := range activeChildren {
		wg.Add(1)
		go func(childID uuid.UUID) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					h.logger.Warn(ctx, "cascadeCancelChildren.panic", map[string]any{
						"parent_session_id": parentSessionID.String(),
						"child_session_id":  childID.String(),
						"panic":             fmt.Sprintf("%v", r),
					})
				}
			}()
			// Per-child timeout: each child gets up to 30s independently.
			childCtx, childCancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
			defer childCancel()
			if err := h.queue.CancelSession(childCtx, childID.String(), "parent session cancelled"); err != nil {
				h.logger.Warn(ctx, "cascadeCancelChildren.cancel_failed", map[string]any{
					"parent_session_id": parentSessionID.String(),
					"child_session_id":  childID.String(),
					"error":             err.Error(),
				})
			}
		}(child.ID)
	}
	wg.Wait()
	h.logger.Info(ctx, "cascadeCancelChildren.done", map[string]any{
		"turn_id":     turnID,
		"session_id":  parentSessionID.String(),
		"child_count": len(activeChildren),
	})
}
