package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"

	"github.com/google/uuid"
)

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
		// via WithSessionID). Without this, the session stays stuck at "active"
		// until the stale turn scanner runs (5-30 min).
		sessionIDStr := turnagent.SessionIDFromContext(ctx)
		if sid, parseErr := uuid.Parse(sessionIDStr); parseErr == nil {
			if err := h.deps.SessionRepo.UpdateStatus(ctx, sid, protocol.SessionStatusIdle); err != nil {
				h.logger.Warn(ctx, "failTurn.update_session_status_failed_fallback", map[string]any{
					"session_id": sid.String(),
					"error":      err.Error(),
				})
			}
			h.batchLifecyclePublish(ctx, tid, sid, "fail")
			// Sub Agent support: notify parent and cascade-cancel children even
			// when GetByID failed. Without this, a sub-agent failure with a DB
			// lookup error leaves the parent session stuck at "active" with no
			// error indication until the stale turn scanner runs (5-30 minutes),
			// and orphaned child sessions continue running.
			h.notifyParentAfterSubAgentSession(ctx, nil, nil, "failed", &errMsg)
			h.cascadeCancelChildren(ctx, turnID, sid)
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
