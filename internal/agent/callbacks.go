package agent

import (
	"context"
	"fmt"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// =============================================================================
// Turn ownership callbacks
// =============================================================================
//
// These two callbacks are the integration point for turn allocation and lookup.
// The old worker package created turns in BuildInput (API layer created them
// before enqueueing). The new turn-agent package owns the turn lifecycle and
// delegates allocation/lookup to the application via these callbacks.

// createTurn allocates a new turn in the DB.
//
// Idempotency: workID is used as the turn's ClientID. If a turn with the same
// ClientID already exists (e.g., after a worker crash and rtc-queue retry),
// the existing turn is returned instead of creating a duplicate.
//
// Mapping from old code: the old worker expected the turn to already exist
// (created by the API layer in SubmitTurn). The new code creates the turn
// here, at the start of the work.
func (h *helpers) createTurn(ctx context.Context, sessionID string, workID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", fmt.Errorf("createTurn: invalid session ID %q: %w", sessionID, err)
	}

	// Idempotency check: if a turn with this ClientID already exists, return it.
	// This handles rtc-queue retries after a worker crash.
	existing, err := h.deps.TurnRepo.FindByClientID(ctx, workID)
	if err != nil {
		return "", fmt.Errorf("createTurn: find existing turn by client_id %q: %w", workID, err)
	}
	if existing != nil {
		h.logIfEnabled(ctx, "createTurn.idempotent_hit", map[string]any{
			"session_id": sessionID,
			"work_id":    workID,
			"turn_id":    existing.ID.String(),
		})
		return existing.ID.String(), nil
	}

	// Create a new turn.
	turn := &model.Turn{
		SessionID: sid,
		ClientID:  workID, // workID as idempotency key
		Status:    string(model.TurnStatusPending),
	}
	if err := h.deps.TurnRepo.Create(ctx, turn); err != nil {
		return "", fmt.Errorf("createTurn: create turn: %w", err)
	}

	h.logIfEnabled(ctx, "createTurn.created", map[string]any{
		"session_id": sessionID,
		"work_id":    workID,
		"turn_id":    turn.ID.String(),
	})

	// Publish turn.created event so the frontend knows about the new turn.
	// This is the ONLY place where turn.created is emitted — all other turn
	// lifecycle transitions (begin, complete, interrupt, resume, fail, cancel)
	// publish turn.updated via publishTurnUpdated.
	// Publishing is best-effort: if it fails, the turn is still created in DB
	// and the frontend will learn about it via subsequent turn.updated events.
	if h.deps.UpdatePublisher != nil {
		session, sessErr := h.deps.SessionRepo.GetByID(ctx, sid)
		if sessErr != nil {
			h.logIfEnabled(ctx, "createTurn.load_session_failed", map[string]any{
				"session_id": sessionID,
				"error":      sessErr.Error(),
			})
		} else {
			updates := primitives.BuildTurnCreatedUpdates(session, turn.ID)
			if len(updates) > 0 {
				if _, err := h.deps.UpdatePublisher.Publish(ctx, updates...); err != nil {
					h.logIfEnabled(ctx, "createTurn.publish_failed", map[string]any{
						"session_id": sessionID,
						"turn_id":    turn.ID.String(),
						"error":      err.Error(),
					})
				}
			}
		}
	}

	return turn.ID.String(), nil
}

// lookupTurn finds the active turn for a session during a resume work.
//
// It queries for turns in "running" or "interrupted" status. If multiple
// exist (shouldn't happen in normal operation), the most recent is returned.
//
// Mapping from old code: the old worker got the turnID from the TurnItem
// pushed into the session's buffer. The new code must look it up from DB
// since the resume work item carries only {kind: "resume", sessionID}.
func (h *helpers) lookupTurn(ctx context.Context, sessionID string, workID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", fmt.Errorf("lookupTurn: invalid session ID %q: %w", sessionID, err)
	}

	active, err := h.deps.TurnRepo.FindActiveBySession(ctx, sid)
	if err != nil {
		return "", fmt.Errorf("lookupTurn: find active turns for session %s: %w", sessionID, err)
	}
	if len(active) == 0 {
		return "", fmt.Errorf("lookupTurn: no active turn for session %s", sessionID)
	}

	// Return the most recent active turn.
	// FindActiveBySession returns turns ordered by created_at ASC, so the
	// last element is the most recent.
	turn := active[len(active)-1]

	h.logIfEnabled(ctx, "lookupTurn.found", map[string]any{
		"session_id": sessionID,
		"work_id":    workID,
		"turn_id":    turn.ID.String(),
		"status":     turn.Status,
	})

	return turn.ID.String(), nil
}

// =============================================================================
// Turn state transition callbacks
// =============================================================================
//
// These six callbacks persist turn state transitions and publish turn.updated
// events. They are called by turn-agent at well-defined points in the turn's
// lifecycle. The application must NOT mutate turn state from other code paths.
//
// Common pattern: each callback updates the turn's status in the DB via
// TurnRepo.UpdateStatus, then publishes a turn.updated event via the
// UpdatePublisher. The DB update and publish are independent: the DB update
// must always succeed; the publish is best-effort (frontend may miss the
// event, but DB state remains correct).

// beginTurn marks a freshly created turn as "running".
//
// Called exactly once per turn, after CreateTurn. The turnID is then passed
// to every subsequent callback for the duration of the turn.
//
// Also sets the parent session's status to "active" to indicate that a turn
// is currently executing.
func (h *helpers) beginTurn(ctx context.Context, turnID string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("beginTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusRunning, ""); err != nil {
		return fmt.Errorf("beginTurn: update status: %w", err)
	}

	// Batch publish: combine turn.updated + session.updated into a single
	// centrifuge call. Load the turn once to get the sessionID, update
	// session status in DB first (so the frontend sees consistent state
	// when it reacts to the event), then publish both events together.
	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "beginTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// DB update session status before publishing events.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusActive); err != nil {
		h.logIfEnabled(ctx, "beginTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "begin")

	h.logIfEnabled(ctx, "beginTurn.done", map[string]any{
		"turn_id": turnID,
	})

	return nil
}

// completeTurn marks a turn as "completed" after a clean exit.
//
// The eino checkpoint has already been deleted by eino's TurnLoop on clean
// exit. This callback persists the "completed" state and notifies frontend.
//
// Also sets the parent session's status to "idle" to indicate no turn is
// currently executing.
func (h *helpers) completeTurn(ctx context.Context, turnID string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("completeTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusCompleted, ""); err != nil {
		return fmt.Errorf("completeTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "completeTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// DB update session status before publishing events.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "completeTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "complete")

	h.logIfEnabled(ctx, "completeTurn.done", map[string]any{"turn_id": turnID})

	return nil
}

// interruptTurn marks a turn as "interrupted" when eino raises a stateful
// interrupt (typically from an RTC tool calling tool.StatefulInterrupt).
//
// interruptID is the eino-assigned ID of the root interrupt context.
// interruptInfo is the opaque info passed to StatefulInterrupt (typically
// rtcInterruptInfo, but the concrete type is application-defined).
//
// After this callback returns, turn-agent's Process returns nil, rtc-queue
// completes the work, and the session lock is released. The application is
// responsible for publishing a Resume work item to rtc-queue when the
// external event resolves (e.g., when SubmitRtcResult is called).
//
// TODO: The interrupt handling in the old code also subscribed to pub/sub
// channels and waited for answers. In the new model, the application's
// interrupt handling (e.g., SubmitRtcResult) is responsible for publishing
// a Resume work item to rtc-queue. The subscribe-and-wait logic is no longer
// needed here because turn-agent manages the turn lifecycle.
func (h *helpers) interruptTurn(ctx context.Context, turnID string, interruptID string, interruptInfo any) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("interruptTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusInterrupted, ""); err != nil {
		return fmt.Errorf("interruptTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "interruptTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status to "idle" — turn is paused waiting for external input.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "interruptTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "interrupt")

	h.logIfEnabled(ctx, "interruptTurn.done", map[string]any{
		"turn_id":      turnID,
		"interrupt_id": interruptID,
		"info_type":    fmt.Sprintf("%T", interruptInfo),
	})

	return nil
}

// resumeTurn marks an interrupted turn as "running" again.
//
// Called after LookupTurn, before eino's TurnLoop re-enters the tool that
// previously interrupted.
//
// Also sets the parent session's status back to "active" to indicate that
// a turn is executing again after an interrupt resolution.
func (h *helpers) resumeTurn(ctx context.Context, turnID string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("resumeTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusRunning, ""); err != nil {
		return fmt.Errorf("resumeTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "resumeTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status back to "active" — a turn is executing again.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusActive); err != nil {
		h.logIfEnabled(ctx, "resumeTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "resume")

	h.logIfEnabled(ctx, "resumeTurn.done", map[string]any{"turn_id": turnID})

	return nil
}

// failTurn marks a turn as "failed" with an error message.
//
// Called when the turn ends due to an unexpected error. The eino checkpoint
// may or may not be present depending on where the failure occurred.
func (h *helpers) failTurn(ctx context.Context, turnID string, turnErr error) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("failTurn: invalid turn ID %q: %w", turnID, err)
	}

	errMsg := ""
	if turnErr != nil {
		errMsg = turnErr.Error()
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusFailed, errMsg); err != nil {
		return fmt.Errorf("failTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "failTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status to "idle" — turn ended due to an error.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "failTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "fail")

	h.logIfEnabled(ctx, "failTurn.done", map[string]any{
		"turn_id": turnID,
		"error":   errMsg,
	})

	return nil
}

// cancelTurn marks a turn as "cancelled".
//
// Called when the turn is cancelled via rtc-queue's Cancel admin operation.
// reason carries the CancelMessage.Reason from the publisher.
func (h *helpers) cancelTurn(ctx context.Context, turnID string, reason string) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("cancelTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusCancelled, reason); err != nil {
		return fmt.Errorf("cancelTurn: update status: %w", err)
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		h.logIfEnabled(ctx, "cancelTurn.load_turn_failed", map[string]any{
			"turn_id": turnID,
			"error":   lookupErr.Error(),
		})
		return nil
	}

	// Set session status to "idle" — turn was cancelled.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logIfEnabled(ctx, "cancelTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "cancel")

	h.logIfEnabled(ctx, "cancelTurn.done", map[string]any{
		"turn_id": turnID,
		"reason":  reason,
	})

	return nil
}
