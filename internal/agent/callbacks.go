package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rtc-agent/server/internal/agent/command"
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
		// Handle unique constraint violation (TOCTOU race): another goroutine
		// created the same turn between our FindByClientID and Create. Re-query
		// to return the existing turn instead of failing.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			existing, lookupErr := h.deps.TurnRepo.FindByClientID(ctx, workID)
			if lookupErr != nil {
				return "", fmt.Errorf("createTurn: duplicate key but re-lookup failed: %w", lookupErr)
			}
			if existing != nil {
				h.logger.Info(ctx, "createTurn.idempotent_hit_race", map[string]any{
					"session_id": sessionID,
					"work_id":    workID,
					"turn_id":    existing.ID.String(),
				})
				return existing.ID.String(), nil
			}
			// Anomalous: unique constraint fired but re-lookup found nothing.
			// Log this TOCTOU edge case so operators can detect DB inconsistency.
			h.logger.Warn(ctx, "createTurn.duplicate_key_relookup_nil", map[string]any{
				"session_id": sessionID,
				"work_id":    workID,
				"message":    "unique constraint violation but re-lookup returned nil; returning original error",
			})
		}
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

// publishLifecycleFallback handles the case where Turn lookup fails during a
// lifecycle callback (beginTurn / failTurn). It extracts the sessionID from
// context (set by agent_process.go via WithSessionID), updates the session
// status, and publishes the lifecycle event so the frontend stays in sync.
//
// Returns nil — the caller treats this as "best-effort completed". The turn
// status was already updated in the DB before this fallback runs.
func (h *helpers) publishLifecycleFallback(
	ctx context.Context,
	tid uuid.UUID,
	turnID string,
	lookupErr error,
	sessionStatus protocol.SessionStatus,
	label string,
) error {
	sessionIDStr := turnagent.SessionIDFromContext(ctx)
	if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
		if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, sessionStatus); err != nil {
			h.logger.Warn(ctx, label+".update_session_status_failed_fallback", map[string]any{
				"session_id": sid.String(),
				"error":      err.Error(),
			})
		}
		h.batchLifecyclePublish(ctx, tid, sid, label)
	}
	h.logger.Warn(ctx, label+".load_turn_failed", map[string]any{
		"turn_id":  turnID,
		"error":    lookupErr.Error(),
		"fallback": sessionIDStr != "",
	})
	return nil
}

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
		// Fallback: beginTurn returns nil here — the turn status was already
		// updated to "running" above. The caller sees success; only the session
		// activation and event publishing use the fallback path.
		return h.publishLifecycleFallback(ctx, tid, turnID, lookupErr,
			protocol.SessionStatusActive, "beginTurn")
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
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, sid)
	if sessErr != nil {
		h.logger.Warn(ctx, "completeTurn.load_session_failed", map[string]any{
			"session_id": sessionID,
			"error":      sessErr.Error(),
		})
		// Continue even if session load fails — turn status is already updated.
	}

	// DB update session status before publishing events.
	// Skip the update if session is already idle (avoid redundant DB write).
	if session != nil && session.Status == string(protocol.SessionStatusIdle) {
		// Already idle — nothing to update.
	} else if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
		h.logger.Warn(ctx, "completeTurn.update_session_status_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
	}

	// Batch publish: turn.updated + session.updated in one centrifuge call.
	h.batchLifecyclePublish(ctx, tid, sid, "complete")

	h.logger.Info(ctx, "completeTurn.done", map[string]any{"turn_id": turnID, "session_id": sessionID})

	// Sub Agent support: if this is a sub session, notify the parent.
	// If session is nil (load failed above), notifyParentAfterSubAgentSession
	// returns early and the parent is NOT notified. The stale turn scanner
	// will eventually clean up, but with a 5-30 minute delay.
	if session == nil {
		h.logger.Warn(ctx, "completeTurn.skip_parent_notification_session_nil", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"message":    "session load failed earlier, parent notification skipped — stale turn scanner will recover",
		})
	}
	h.notifyParentAfterSubAgentSession(ctx, session, lastMessage, "completed", nil)

	// Cascade cancel: if this completed session has active child sessions (sub agents),
	// cancel them as well. This prevents orphaned sub agents from continuing to run
	// after the parent has completed. Mirrors failTurn and cancelTurn.
	h.cascadeCancelChildren(ctx, turnID, sid)

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

// =============================================================================
// Shared fallback helpers
// =============================================================================

// fallbackTurnLookupCleanup handles the common cleanup when TurnRepo.GetByID
// fails in cancelTurn/failTurn. It extracts the sessionID from context (set by
// agent_process.go via WithSessionID) and performs session status update,
// lifecycle event publishing, parent notification, and cascade cancel.
//
// Parameters:
//   - ctx: callback context
//   - turnID: original turn ID string
//   - tid: parsed turn UUID
//   - action: lifecycle publish action ("cancel" or "fail")
//   - subAgentStatus: sub-agent status string ("cancelled" or "failed")
//   - errorMsg: pointer to error/reason message for parent notification
//   - logPrefix: caller name for log events ("cancelTurn" or "failTurn")
func (h *helpers) fallbackTurnLookupCleanup(ctx context.Context, turnID string, tid uuid.UUID, action string, subAgentStatus string, errorMsg *string, logPrefix string) {
	sessionIDStr := turnagent.SessionIDFromContext(ctx)
	sid, parseErr := uuid.Parse(sessionIDStr)
	if parseErr != nil {
		h.logger.Warn(ctx, logPrefix+".load_turn_failed", map[string]any{
			"turn_id":  turnID,
			"fallback": false,
		})
		return
	}

	if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
		h.logger.Warn(ctx, logPrefix+".update_session_status_failed_fallback", map[string]any{
			"session_id": sid.String(),
			"error":      err.Error(),
		})
	}
	h.batchLifecyclePublish(ctx, tid, sid, action)

	// Sub Agent support: query session by sessionID (not turnID) to check if
	// this is a sub-session and notify the parent. Even though Turn lookup
	// failed, Session lookup may succeed (different table). Without this, a
	// sub-agent failure/cancellation with a Turn DB lookup error leaves the
	// parent session stuck at "active" until the stale turn scanner runs.
	session, sessErr := h.deps.SessionRepo.GetByID(ctx, sid)
	if sessErr != nil {
		h.logger.Warn(ctx, logPrefix+".load_session_fallback_failed", map[string]any{
			"session_id": sid.String(),
			"error":      sessErr.Error(),
		})
	}
	h.notifyParentAfterSubAgentSession(ctx, session, nil, subAgentStatus, errorMsg)
	// Cascade cancel: propagate to active child sessions even when GetByID
	// failed. This prevents orphaned sub agents.
	h.cascadeCancelChildren(ctx, turnID, sid)

	h.logger.Warn(ctx, logPrefix+".load_turn_failed", map[string]any{
		"turn_id":  turnID,
		"fallback": true,
	})
}
