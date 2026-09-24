package rtcqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// lockRenewalState tracks whether the lock has been lost during work processing.
type lockRenewalState struct {
	lost *atomic.Bool
	done chan struct{}
}

// setupCancelListener configures cancel notification subscription with
// dual safety-net checks to prevent missed cancel signals.
func (w *Worker) setupCancelListener(
	workCtx context.Context,
	claim *ClaimResult,
	work *Work,
	cancelCh chan CancelMessage,
	adminCancelled *atomic.Bool,
	workCancel context.CancelFunc,
) {
	// Safety-net 1: check if work was already cancelled before subscribing.
	if work.Status == StatusCancelled {
		w.log(workCtx, "worker.cancelled_before_subscribe", map[string]any{
			"work_id":    claim.WorkID,
			"session_id": work.SessionID,
			"message":    "work was cancelled before cancel subscription was set up; triggering cancel now",
		})
		// Non-blocking send: cancelCh has buffer 1, but use select to prevent
		// a theoretical deadlock if the buffer is already full (e.g., from a
		// concurrent safety-net 2 delivery). Mirrors the subscription handler's
		// non-blocking pattern at lines 63-66.
		select {
		case cancelCh <- CancelMessage{
			WorkID:    claim.WorkID,
			Reason:    "cancelled_before_subscribe",
			Timestamp: time.Now().Unix(),
		}:
		default:
		}
		adminCancelled.Store(true)
		workCancel()
	}

	// Subscribe to cancel notifications.
	// If safety-net 1 already triggered cancel, workCtx is already cancelled.
	// SubscribeCancel returns a subscription that won't deliver messages, but
	// the cancel is already in cancelCh and adminCancelled is already set.
	cancelSub := w.q.SubscribeCancel(workCtx, claim.SessionID)
	go func() {
		defer func() { _ = cancelSub.Close() }()
		defer func() {
			if r := recover(); r != nil {
				w.logError(workCtx, "cancel listener panic",
					"session", claim.SessionID, "recover", r, "stack", string(debug.Stack()))
			}
		}()
		for msg := range cancelSub.Channel() {
			var cm CancelMessage
			if err := json.Unmarshal([]byte(msg.Payload), &cm); err != nil {
				w.log(workCtx, "worker.cancel_msg_unmarshal_failed", map[string]any{
					"session_id": claim.SessionID,
					"work_id":    claim.WorkID,
					"error":      err.Error(),
				})
				continue
			}
			w.log(workCtx, "worker.cancel_msg_received", map[string]any{
				"session_id":    claim.SessionID,
				"work_id":       claim.WorkID,
				"msg_work_id":   cm.WorkID,
				"msg_reason":    cm.Reason,
				"work_id_match": cm.WorkID == claim.WorkID,
			})
			if cm.WorkID == claim.WorkID {
				select {
				case cancelCh <- cm:
				default:
				}
				adminCancelled.Store(true)
				workCancel()
				w.log(workCtx, "worker.cancel_msg_delivered", map[string]any{
					"session_id": claim.SessionID,
					"work_id":    claim.WorkID,
					"reason":     cm.Reason,
				})
				return
			}
		}
		// Channel closed without delivering a matching cancel message.
		// This means the Pub/Sub subscription ended (workCtx cancelled or
		// connection lost) before a cancel for this work arrived.
		w.log(workCtx, "worker.cancel_sub_channel_closed", map[string]any{
			"session_id":      claim.SessionID,
			"work_id":         claim.WorkID,
			"admin_cancelled": adminCancelled.Load(),
			"message":         "cancel subscription channel closed; Pub/Sub subscription may have been lost",
		})
	}()

	// Safety-net 2: re-check after subscribing to cover the race window
	// where CancelSession runs between LoadWork and SubscribeCancel.
	if !adminCancelled.Load() {
		recheck, recheckErr := w.q.LoadWork(workCtx, claim.WorkID)
		if recheckErr == nil && recheck != nil && recheck.Status == StatusCancelled {
			w.log(workCtx, "worker.cancelled_after_subscribe", map[string]any{
				"work_id":    claim.WorkID,
				"session_id": claim.SessionID,
				"message":    "work was cancelled between LoadWork and SubscribeCancel; Pub/Sub message was lost; triggering cancel now",
			})
			// Non-blocking send: mirrors safety-net 1's pattern (lines 39-46)
			// and the subscription handler (lines 70-73). cancelCh has buffer 1;
			// the select/default guards against a theoretical race where safety-net 1
			// or the subscription handler already filled the buffer between the
			// adminCancelled guard check and this point.
			select {
			case cancelCh <- CancelMessage{
				WorkID:    claim.WorkID,
				Reason:    "cancelled_race_detected",
				Timestamp: time.Now().Unix(),
			}:
			default:
			}
			adminCancelled.Store(true)
			workCancel()
		}
	}
}

// startLockRenewal launches the background lock renewal goroutine.
// Returns a state struct with a lost flag and done channel for cleanup.
func (w *Worker) startLockRenewal(
	workCtx context.Context,
	claim *ClaimResult,
	holdLock bool,
	credential string,
	workCancel context.CancelFunc,
) lockRenewalState {
	var lost atomic.Bool
	state := lockRenewalState{
		lost: &lost,
		done: make(chan struct{}),
	}
	var consecutiveRenewFailures atomic.Int64

	go func() {
		defer func() {
			if r := recover(); r != nil {
				w.logError(workCtx, "lock renewal panic",
					"session", claim.SessionID, "recover", r, "stack", string(debug.Stack()))
			}
		}()
		t := time.NewTicker(w.cfg.RenewInterval)
		defer t.Stop()
		for {
			select {
			case <-state.done:
				return
			case <-workCtx.Done():
				return
			case <-t.C:
				var ok bool
				var err error
				if holdLock && credential != "" {
					ok, err = w.q.RenewLockWithCredential(workCtx, claim.SessionID, w.cfg.WorkerID, credential)
				} else {
					ok, err = w.q.RenewLock(workCtx, claim.SessionID, w.cfg.WorkerID)
				}
				if err != nil {
					failures := consecutiveRenewFailures.Add(1)
					w.log(workCtx, "worker.renewal_transient_error", map[string]any{
						"session_id":           claim.SessionID,
						"consecutive_failures": failures,
						"error":                err.Error(),
					})
					if failures >= DefaultMaxConsecutiveRenewFailures {
						w.logError(workCtx, "worker.renewal_giving_up",
							"session", claim.SessionID,
							"consecutive_failures", failures,
						)
						state.lost.Store(true)
						workCancel()
						return
					}
					continue
				}
				consecutiveRenewFailures.Store(0)
				if !ok {
					w.log(workCtx, "worker.lock_lost", map[string]any{
						"session_id": claim.SessionID,
						"work_id":    claim.WorkID,
					})
					state.lost.Store(true)
					workCancel()
					return
				}
			}
		}
	}()
	return state
}

// handleWorkCompletion processes the result of OnWork callback.
func (w *Worker) handleWorkCompletion(
	claim *ClaimResult,
	onworkErr error,
	holdLock bool,
	lockLost *atomic.Bool,
	adminCancelled *atomic.Bool,
) {
	if lockLost.Load() {
		// another worker took over; do NOT call Complete (it would release
		// someone else's lock). The lock will expire naturally.
		return
	}
	if adminCancelled.Load() {
		// admin cancelled; the cancel script already set status=cancelled
		// and released the lock. Do NOT call Complete.
		return
	}
	if onworkErr != nil {
		w.cfg.OnError(fmt.Errorf("onwork %s: %w", claim.WorkID, onworkErr))
		// leave work in "processing" state for manual recovery
		return
	}

	// success — complete the work.
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer completeCancel()

	if holdLock {
		if err := w.q.CompleteWork(completeCtx, claim.WorkID); err != nil {
			w.cfg.OnError(fmt.Errorf("complete work %s: %w", claim.WorkID, err))
		}
	} else {
		if err := w.q.Complete(completeCtx, claim.WorkID); err != nil {
			w.cfg.OnError(fmt.Errorf("complete %s: %w", claim.WorkID, err))
		}
	}
}
