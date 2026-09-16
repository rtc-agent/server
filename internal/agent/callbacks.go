package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/agent/command"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// hsetExpireScript atomically sets multiple hash fields and refreshes the key TTL.
// KEYS[1] = hash key
// ARGV[1..N-1] = field/value pairs (must be even count)
// ARGV[N] = TTL in seconds
// Returns: number of fields added (HSET return value).
var hsetExpireScript = redis.NewScript(`
local n = #ARGV
local ttl = tonumber(ARGV[n])
for i = 1, n - 1, 2 do
    redis.call("HSET", KEYS[1], ARGV[i], ARGV[i+1])
end
redis.call("EXPIRE", KEYS[1], ttl)
return 1
`)

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
		h.logger.Info(ctx, "createTurn.idempotent_hit", map[string]any{
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

	h.logger.Info(ctx, "createTurn.created", map[string]any{
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
			h.logger.Warn(ctx, "createTurn.load_session_failed", map[string]any{
				"session_id": sessionID,
				"error":      sessErr.Error(),
			})
		} else {
			updates := primitives.BuildTurnCreatedUpdates(session, turn.ID)
			if len(updates) > 0 {
				if _, err := h.deps.UpdatePublisher.Publish(ctx, updates...); err != nil {
					h.logger.Warn(ctx, "createTurn.publish_failed", map[string]any{
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
// It queries for turns in "pending", "running", or "interrupted" status. If
// multiple exist (shouldn't happen in normal operation), the most recent is
// returned (ordered by created_at ASC).
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
		// Wrap with ErrNoActiveTurn so Process can detect this specific case
		// and complete the work gracefully (turn was cancelled/completed).
		return "", fmt.Errorf("lookupTurn: %w: %s", turnagent.ErrNoActiveTurn, sessionID)
	}

	// Return the most recent active turn.
	// FindActiveBySession returns turns ordered by created_at ASC, so the
	// last element is the most recent.
	turn := active[len(active)-1]

	h.logger.Info(ctx, "lookupTurn.found", map[string]any{
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
		// Fallback: extract sessionID from context (set by agent_process.go
		// via WithSessionID before calling BeginTurn). This ensures session
		// activation and event publishing still work even when DB lookup fails.
		// NOTE: beginTurn returns nil here — the turn status was already updated
		// to "running" above. The caller sees success; only the session activation
		// and event publishing use the fallback path.
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
			if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusActive); err != nil {
				h.logger.Warn(ctx, "beginTurn.update_session_status_failed_fallback", map[string]any{
					"session_id": sid.String(),
					"error":      err.Error(),
				})
			}
			h.batchLifecyclePublish(ctx, tid, sid, "begin")
		}
		h.logger.Warn(ctx, "beginTurn.load_turn_failed", map[string]any{
			"turn_id":  turnID,
			"error":    lookupErr.Error(),
			"fallback": sessionIDStr != "",
		})
		return nil
	}

	// DB update session status before publishing events.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusActive); err != nil {
		h.logger.Warn(ctx, "beginTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "begin")

	h.logger.Info(ctx, "beginTurn.done", map[string]any{
		"turn_id": turnID,
	})

	// Sub Agent support: if this is a sub session, update the parent's sub_agent_invocation message status to "running".
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, turn.SessionID)
	if sessErr == nil && session.ParentServerSessionID != uuid.Nil && session.SubAgentParentMessageID != uuid.Nil {
		h.updateSubAgentInvocationStatus(ctx, session.SubAgentParentMessageID, "running")
	}

	return nil
}

// completeTurn marks a turn as "completed" after a clean exit.
//
// The eino checkpoint has already been deleted by eino's TurnLoop on clean
// exit. This callback persists the "completed" state and notifies frontend.
//
// Also sets the parent session's status to "idle" to indicate no turn is
// currently executing.
//
// Sub Agent support: if the session has a ParentServerSessionID (is a sub
// session), this callback also triggers the parent session's resume by
// publishing a Resume work item to the parent's rtc-queue, carrying the sub
// session's final result.
func (h *helpers) completeTurn(ctx context.Context, sessionID string, turnID string, lastMessage *turnagent.Message) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("completeTurn: invalid turn ID %q: %w", turnID, err)
	}

	if err := h.deps.TurnRepo.UpdateStatus(ctx, tid, protocol.TurnStatusCompleted, ""); err != nil {
		return fmt.Errorf("completeTurn: update status: %w", err)
	}

	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("completeTurn: invalid session ID %q: %w", sessionID, err)
	}

	// Load session to check Sub Agent hierarchy and publish events.
	session, sessionErr := h.deps.SessionRepo.GetByID(ctx, sid)
	if sessionErr != nil {
		h.logger.Warn(ctx, "completeTurn.load_session_failed", map[string]any{
			"session_id": sessionID,
			"error":      sessionErr.Error(),
		})
		// Continue even if session load fails — turn status is already updated.
	}

	// DB update session status before publishing events.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
		h.logger.Warn(ctx, "completeTurn.update_session_status_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, sid, "complete")

	h.logger.Info(ctx, "completeTurn.done", map[string]any{"turn_id": turnID, "session_id": sessionID})

	// Sub Agent support: if this is a sub session, notify the parent.
	h.notifyParentAfterSubAgentSession(ctx, session, lastMessage, "completed", nil)

	// Slash-command framework: notify active commands of turn completion.
	// The /goal execution loop is now handled by GoalWorkflow.OnTurnComplete
	// (see goal_workflow.go) via the registry.
	if h.deps.CommandRegistry != nil {
		cmdCtx := command.Context{
			Context:   ctx,
			SessionID: sid,
			TurnID:    tid,
		}
		if errs := h.deps.CommandRegistry.OnTurnComplete(cmdCtx); len(errs) > 0 {
			for _, e := range errs {
				h.logger.Warn(ctx, "completeTurn.command_hook_failed", map[string]any{
					"session_id": sessionID,
					"turn_id":    turnID,
					"error":      e.Error(),
				})
			}
		}
	}

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
func (h *helpers) interruptTurn(ctx context.Context, turnID string, interruptID string, interruptInfo any, allInterruptContexts []*turnagent.InterruptContext) error {
	tid, err := uuid.Parse(turnID)
	if err != nil {
		return fmt.Errorf("interruptTurn: invalid turn ID %q: %w", turnID, err)
	}

	// Atomically update status AND interrupt ID in a single DB write.
	// This prevents a race condition where SubmitRtcResult could read the
	// interrupted status before InterruptID is persisted.
	if err := h.deps.TurnRepo.UpdateStatusAndInterruptID(ctx, tid, protocol.TurnStatusInterrupted, interruptID); err != nil {
		return fmt.Errorf("interruptTurn: update status and interrupt_id: %w", err)
	}

	// For batch interrupts: store the mapping from RtcID to eino's InterruptCtx.ID
	// This is needed because ResumeParams.Targets uses eino's internal IDs, not RtcIDs.
	if len(allInterruptContexts) > 1 && h.deps.Redis != nil {
		turnUUID, parseErr := uuid.Parse(turnID)
		if parseErr == nil {
			interruptMapKey := cache.RtcBatchInterruptMap(turnUUID.String())
			// Collect all RtcID -> InterruptCtx.ID pairs for atomic HSET + EXPIRE.
			var args []interface{}
			for _, ic := range allInterruptContexts {
				if info, ok := ic.Info.(rtcInterruptInfo); ok && info.RtcID != "" {
					args = append(args, info.RtcID, ic.ID)
				}
			}
			if len(args) > 0 {
				pairCount := len(args) / 2                           // number of RtcID→InterruptCtx.ID pairs
				args = append(args, int(10*time.Minute/time.Second)) // TTL as last ARGV
				if err := hsetExpireScript.Run(ctx, h.deps.Redis, []string{interruptMapKey}, args...).Err(); err != nil {
					h.logger.Warn(ctx, "interruptTurn.batch_interrupt_mapping_failed", map[string]any{
						"turn_id": turnID,
						"error":   err.Error(),
					})
				} else {
					h.logger.Info(ctx, "interruptTurn.batch_interrupt_mapping_stored", map[string]any{
						"turn_id":         turnID,
						"interrupt_count": pairCount,
					})
				}
			}
		}
	}

	turn, lookupErr := h.deps.TurnRepo.GetByID(ctx, tid)
	if lookupErr != nil {
		// Fallback: extract sessionID from context (set by agent_process.go
		// via WithSessionID). Ensures event publishing still works when DB
		// lookup fails.
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
			h.batchLifecyclePublish(ctx, tid, sid, "interrupt")
		}
		h.logger.Warn(ctx, "interruptTurn.load_turn_failed", map[string]any{
			"turn_id":  turnID,
			"error":    lookupErr.Error(),
			"fallback": sessionIDStr != "",
		})
		return nil
	}

	// Set session status to "idle" — turn is paused waiting for external input.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logger.Warn(ctx, "interruptTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "interrupt")

	h.logger.Info(ctx, "interruptTurn.done", map[string]any{
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
		// Fallback: extract sessionID from context (set by agent_process.go
		// via WithSessionID). Session status was already updated above.
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
			h.batchLifecyclePublish(ctx, tid, sid, "resume")
		}
		h.logger.Warn(ctx, "resumeTurn.load_turn_failed", map[string]any{
			"turn_id":  turnID,
			"error":    lookupErr.Error(),
			"fallback": sessionIDStr != "",
		})
		return nil
	}

	// Set session status back to "active" — a turn is executing again.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusActive); err != nil {
		h.logger.Warn(ctx, "resumeTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "resume")

	h.logger.Info(ctx, "resumeTurn.done", map[string]any{"turn_id": turnID})

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
		// Fallback: extract sessionID from context (set by agent_process.go
		// via WithSessionID). Ensures session status update and event
		// publishing still work when DB lookup fails. Without this, the
		// session stays stuck at "active" until the stale turn scanner
		// runs (5-30 minutes).
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
			if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
				h.logger.Warn(ctx, "failTurn.update_session_status_failed_fallback", map[string]any{
					"session_id": sid.String(),
					"error":      err.Error(),
				})
			}
			h.batchLifecyclePublish(ctx, tid, sid, "fail")
		}
		h.logger.Warn(ctx, "failTurn.load_turn_failed", map[string]any{
			"turn_id":  turnID,
			"error":    lookupErr.Error(),
			"fallback": sessionIDStr != "",
		})
		return nil
	}

	// Set session status to "idle" — turn ended due to an error.
	if err := h.deps.SessionRepo.UpdateStatus(ctx, turn.SessionID, protocol.SessionStatusIdle); err != nil {
		h.logger.Warn(ctx, "failTurn.update_session_status_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, turn.SessionID, "fail")

	h.logger.Info(ctx, "failTurn.done", map[string]any{
		"turn_id": turnID,
		"error":   errMsg,
	})

	// Insert error feedback message for the user.
	// Skipped when:
	//   - turnErr is nil (nothing to report)
	//   - error is context.Canceled (normal cancellation, not an error)
	//   - context carries the skip flag (caller already inserted a message)
	if turnErr != nil &&
		!errors.Is(turnErr, context.Canceled) &&
		!turnagent.ShouldSkipErrorMessage(ctx) {
		category, title, message, retryable := classifyError(turnErr)
		if err := h.insertErrorMessage(ctx, turn.SessionID, &tid,
			category, title, message, retryable, turnErr.Error()); err != nil {
			h.logger.Warn(ctx, "failTurn.insertErrorMessage_failed", map[string]any{
				"turn_id":    turnID,
				"session_id": turn.SessionID.String(),
				"error":      err.Error(),
			})
			// Do not block failTurn flow on error message insertion failure.
		}
	}

	// Sub Agent support: if this is a sub session, notify the parent.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, turn.SessionID)
	if sessErr != nil {
		h.logger.Warn(ctx, "failTurn.load_session_failed", map[string]any{
			"session_id": turn.SessionID.String(),
			"error":      sessErr.Error(),
		})
	}
	h.notifyParentAfterSubAgentSession(ctx, session, nil, "failed", &errMsg)

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
		// Fallback: extract sessionID from context (set by agent_process.go
		// via WithSessionID). Ensures session status update, event publishing,
		// and cascade cancel still work when DB lookup fails. Without this,
		// the session stays stuck at "active" until the stale turn scanner
		// runs (5-30 minutes).
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
			if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
				h.logger.Warn(ctx, "cancelTurn.update_session_status_failed_fallback", map[string]any{
					"session_id": sid.String(),
					"error":      err.Error(),
				})
			}
			h.batchLifecyclePublish(ctx, tid, sid, "cancel")
			// Cascade cancel: propagate to active child sessions even when
			// GetByID failed. This prevents orphaned sub agents.
			if h.queue != nil {
				activeChildren, findErr := h.deps.SessionRepo.FindActiveByParent(ctx, sid)
				if findErr == nil && len(activeChildren) > 0 {
					h.logger.Info(ctx, "cancelTurn.cascade_cancel_start_fallback", map[string]any{
						"turn_id":     turnID,
						"session_id":  sid.String(),
						"child_count": len(activeChildren),
					})
					for _, child := range activeChildren {
						if err := h.queue.CancelSession(ctx, child.ID.String(), "parent session cancelled"); err != nil {
							h.logger.Warn(ctx, "cancelTurn.cascade_cancel_failed", map[string]any{
								"parent_session_id": sid.String(),
								"child_session_id":  child.ID.String(),
								"error":             err.Error(),
							})
						}
					}
				}
			}
		}
		h.logger.Warn(ctx, "cancelTurn.load_turn_failed", map[string]any{
			"turn_id":  turnID,
			"error":    lookupErr.Error(),
			"fallback": sessionIDStr != "",
		})
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
	h.notifyParentAfterSubAgentSession(ctx, session, nil, "cancelled", &reason)

	// Cascade cancel: if this session has active child sessions (sub agents),
	// cancel them as well. This prevents orphaned sub agents from continuing
	// to run after the parent has been cancelled.
	if sessErr == nil && h.queue != nil {
		activeChildren, findErr := h.deps.SessionRepo.FindActiveByParent(ctx, turn.SessionID)
		if findErr == nil && len(activeChildren) > 0 {
			h.logger.Info(ctx, "cancelTurn.cascade_cancel_start", map[string]any{
				"turn_id":     turnID,
				"session_id":  turn.SessionID.String(),
				"child_count": len(activeChildren),
			})
			for _, child := range activeChildren {
				if err := h.queue.CancelSession(ctx, child.ID.String(), "parent session cancelled"); err != nil {
					h.logger.Warn(ctx, "cancelTurn.cascade_cancel_failed", map[string]any{
						"parent_session_id": turn.SessionID.String(),
						"child_session_id":  child.ID.String(),
						"error":             err.Error(),
					})
				}
			}
			h.logger.Info(ctx, "cancelTurn.cascade_cancel_done", map[string]any{
				"turn_id":     turnID,
				"session_id":  turn.SessionID.String(),
				"child_count": len(activeChildren),
			})
		}
	}

	return nil
}
