// Package rtcqueue - Queue lifecycle operations.
// This file contains operations for managing work item lifecycle:
// completing, canceling, renewing locks, and releasing sessions.
package rtcqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Complete marks a Work item as completed and releases the session lock.
// Both writes happen in a single Lua script.
//
// Ownership note: the session lock is derived from the work item's
// stored session_id, not from any caller-supplied argument. Callers
// MUST only invoke Complete on work items they themselves claimed —
// completing someone else's work will yank the lock out from under the
// processing worker. This contract is not enforced server-side.
func (q *Queue) Complete(ctx context.Context, workID string) error {
	ctx, span := queueTracer().Start(ctx, "queue.complete",
		trace.WithAttributes(
			attribute.String("work.id", workID),
		),
	)
	defer span.End()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	completeCtx, cancel := context.WithTimeout(ctx, CompleteTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the Lua script is idempotent: if the work
	// is already completed, it will return 0 (not found).
	var n int
	_, err := WithRetry(completeCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		n, runErr = completeScript.Run(ctx, q.rdb, []string{keyWork(workID)},
			time.Now().Unix(),
			ChannelSessionNew,
		).Int()
		return struct{}{}, runErr
	})
	if err != nil {
		globalQueueMetrics.recordComplete("error")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("rtcqueue: complete: %w", err)
	}
	if n == 0 {
		globalQueueMetrics.recordComplete("error")
		span.SetAttributes(attribute.Bool("work.found", false))
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
	globalQueueMetrics.recordComplete("success")
	return nil
}

// CompleteWork marks a Work item as completed WITHOUT releasing the session lock.
// Used in "hold lock" mode where the worker continues processing subsequent work
// items for the same session. The worker should call ReleaseSession when it's
// done processing all work items and wants to release the lock.
//
// This is different from Complete, which releases the lock and allows other
// workers to claim the next work item.
func (q *Queue) CompleteWork(ctx context.Context, workID string) error {
	ctx, span := queueTracer().Start(ctx, "queue.complete_work",
		trace.WithAttributes(
			attribute.String("work.id", workID),
		),
	)
	defer span.End()

	// First, load the work to get sessionID
	work, err := q.LoadWork(ctx, workID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("rtcqueue: complete work: load work: %w", err)
	}
	if work == nil {
		span.SetAttributes(attribute.Bool("work.found", false))
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
	span.SetAttributes(attribute.String("session.id", work.SessionID))

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	completeCtx, cancel := context.WithTimeout(ctx, CompleteTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the Lua script is idempotent: if the work
	// is already completed, it will return 0 (not found).
	var n int
	var retryErr error
	_, retryErr = WithRetry(completeCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		n, runErr = completeWorkScript.Run(ctx, q.rdb, []string{
			keyWork(workID),
			keyActive(work.SessionID),
		}, time.Now().Unix()).Int()
		return struct{}{}, runErr
	})
	if retryErr != nil {
		globalQueueMetrics.recordComplete("error")
		span.SetStatus(codes.Error, retryErr.Error())
		return fmt.Errorf("rtcqueue: complete work: %w", retryErr)
	}
	if n == 0 {
		globalQueueMetrics.recordComplete("error")
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
	globalQueueMetrics.recordComplete("success")
	return nil
}

// Cancel cancels a Work item. Pending items are removed from the
// session queue; processing items trigger a cancel notification so the
// owning worker can abort. The session lock is released. All writes
// happen in a single Lua script.
//
// Cancel is an ADMIN operation: any caller may cancel any work item,
// and doing so unconditionally releases the session lock — even if a
// worker is currently processing the item. The in-flight worker is
// expected to notice the cancel notification on
// session:cancel:<session_id> and stop. Do not expose this method to
// untrusted callers.
func (q *Queue) Cancel(ctx context.Context, workID, reason string) error {
	ctx, span := queueTracer().Start(ctx, "queue.cancel",
		trace.WithAttributes(
			attribute.String("work.id", workID),
			attribute.String("cancel.reason", reason),
		),
	)
	defer span.End()

	msg := CancelMessage{
		WorkID:    workID,
		Reason:    reason,
		Timestamp: time.Now().Unix(),
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("rtcqueue: marshal cancel: %w", err)
	}

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	cancelCtx, cancel := context.WithTimeout(ctx, CancelTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the Lua script is idempotent: if the work
	// is already cancelled, it will return -1 (already terminal).
	var n int
	_, err = WithRetry(cancelCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		n, runErr = cancelScript.Run(ctx, q.rdb, []string{keyWork(workID)},
			msg.Timestamp,
			string(payload),
		).Int()
		return struct{}{}, runErr
	})
	if err != nil {
		globalQueueMetrics.recordCancel("error")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("rtcqueue: cancel: %w", err)
	}
	if n == 0 {
		globalQueueMetrics.recordCancel("not_found")
		span.SetAttributes(attribute.Bool("work.found", false))
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
	if n < 0 {
		globalQueueMetrics.recordCancel("already_terminal")
		span.SetAttributes(attribute.Bool("work.already_terminal", true))
		return ErrAlreadyTerminal
	}
	globalQueueMetrics.recordCancel(reason)
	return nil
}

// CancelSession cancels all pending work items for a session and sends a cancel
// signal to the worker currently processing work for this session (if any).
//
// This is used by the API layer to stop all activity for a session (e.g., user
// clicks "stop" or "close session").
//
// The method is safe to call even if no work is pending or processing: it
// returns nil in that case.
func (q *Queue) CancelSession(ctx context.Context, sessionID, reason string) error {
	ctx, span := queueTracer().Start(ctx, "queue.cancel_session",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("cancel.reason", reason),
		),
	)
	defer span.End()

	if sessionID == "" {
		span.SetStatus(codes.Error, "session_id required")
		return fmt.Errorf("rtcqueue: session_id required")
	}

	now := time.Now().Unix()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	cancelCtx, cancel := context.WithTimeout(ctx, CancelTimeout)
	defer cancel()

	// 1. Atomically remove all pending work items from the session queue and
	// mark them as cancelled.
	// Wrap with retry for transient errors. This is safe because the script
	// is idempotent: if items are already cancelled, they won't be found.
	_, err := func() (interface{}, error) {
		var res interface{}
		_, retryErr := WithRetry(cancelCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
			var runErr error
			res, runErr = cancelSessionScript.Run(ctx, q.rdb, []string{keyQueue(sessionID)},
				now,
			).Result()
			return struct{}{}, runErr
		})
		return res, retryErr
	}()
	if err != nil && !errors.Is(err, redis.Nil) {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("rtcqueue: cancel session pending: %w", err)
	}

	// 2. Cancel the work item currently being processed (if any). The
	// cancelSessionActiveScript reads the session:active pointer, publishes
	// a cancel notification, and releases both the lock and the pointer —
	// all atomically. If no work is active the script returns nil and we
	// are done.
	// Wrap with retry for transient errors. This is safe because the script
	// is idempotent: if the work is already cancelled, the pointer is gone.
	_, err = func() (interface{}, error) {
		var res interface{}
		_, retryErr := WithRetry(cancelCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
			var runErr error
			res, runErr = cancelSessionActiveScript.Run(ctx, q.rdb, []string{
				keyLock(sessionID),
				keyActive(sessionID),
				ChannelSessionCancel(sessionID),
			}, reason, now).Result()
			return struct{}{}, runErr
		})
		return res, retryErr
	}()
	if err != nil && !errors.Is(err, redis.Nil) {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("rtcqueue: cancel session active: %w", err)
	}

	return nil
}

// RenewLock refreshes the session lock TTL atomically. Workers should
// call this on a steady interval (see DefaultRenewIntervalSec) while
// processing a task. Returns false if the lock is no longer held by
// this worker (either expired or taken by another worker). The check
// and EXPIRE run in a single Lua script to prevent a TOCTOU race where
// the lock expires between the GET and the EXPIRE and is re-acquired
// by another worker — extending that other worker's hold would corrupt
// ownership.
func (q *Queue) RenewLock(ctx context.Context, sessionID, workerID string) (bool, error) {
	ctx, span := queueTracer().Start(ctx, "queue.renew_lock",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("worker.id", workerID),
		),
	)
	defer span.End()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	renewCtx, cancel := context.WithTimeout(ctx, RenewLockTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the script is idempotent: if the lock is
	// lost, the script will return 0.
	var n int
	_, err := WithRetry(renewCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		n, runErr = renewLockScript.Run(ctx, q.rdb, []string{keyLock(sessionID)},
			workerID, DefaultLockTTLSeconds,
		).Int()
		return struct{}{}, runErr
	})
	if errors.Is(err, redis.Nil) {
		span.SetAttributes(attribute.Bool("lock.renewed", false))
		return false, nil
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("rtcqueue: renew lock: %w", err)
	}
	span.SetAttributes(attribute.Bool("lock.renewed", n == 1))
	return n == 1, nil
}

// RenewLockWithCredential refreshes the session lock TTL atomically for
// hash-based locks used in "hold lock" mode. Returns false if the lock
// is no longer held by this worker with the correct credential.
func (q *Queue) RenewLockWithCredential(ctx context.Context, sessionID, workerID, credential string) (bool, error) {
	ctx, span := queueTracer().Start(ctx, "queue.renew_lock_with_credential",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("worker.id", workerID),
		),
	)
	defer span.End()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	renewCtx, cancel := context.WithTimeout(ctx, RenewLockTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the script is idempotent: if the lock is
	// lost or credential mismatches, the script will return 0.
	var n int
	_, err := WithRetry(renewCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		n, runErr = renewLockWithCredentialScript.Run(ctx, q.rdb, []string{keyLock(sessionID)},
			workerID, credential, DefaultLockTTLSeconds,
		).Int()
		return struct{}{}, runErr
	})
	if errors.Is(err, redis.Nil) {
		span.SetAttributes(attribute.Bool("lock.renewed", false))
		return false, nil
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("rtcqueue: renew lock with credential: %w", err)
	}
	span.SetAttributes(attribute.Bool("lock.renewed", n == 1))
	return n == 1, nil
}

// CompleteWorkAndClaimNext atomically completes the current work item and
// attempts to claim the next pending work item for the same session.
// It verifies credential ownership via the hash-based session lock before
// performing the operation, ensuring only the legitimate worker can complete
// and claim in "hold lock" mode.
//
// The sessionID is derived from the current work item's stored data, so the
// caller does not need to pass it explicitly.
//
// Returns (nil, nil) when there is no more work in the queue — the active
// pointer is cleared in this case.
// Returns (*ClaimResult, nil) when the next work item was successfully claimed.
// Returns (nil, error) on Redis or data errors.
func (q *Queue) CompleteWorkAndClaimNext(ctx context.Context, currentWorkID, workerID, credential string) (*ClaimResult, error) {
	ctx, span := queueTracer().Start(ctx, "queue.complete_work_and_claim_next",
		trace.WithAttributes(
			attribute.String("work.id", currentWorkID),
			attribute.String("worker.id", workerID),
		),
	)
	defer span.End()

	// Load current work to get sessionID
	currentWork, err := q.LoadWork(ctx, currentWorkID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("rtcqueue: complete and claim: load work: %w", err)
	}
	if currentWork == nil {
		span.SetAttributes(attribute.Bool("work.found", false))
		return nil, fmt.Errorf("rtcqueue: work %s not found", currentWorkID)
	}
	span.SetAttributes(attribute.String("session.id", currentWork.SessionID))

	now := time.Now().Unix()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	completeClaimCtx, cancel := context.WithTimeout(ctx, CompleteTimeout)
	defer cancel()

	res, err := completeWorkAndClaimNextScript.Run(completeClaimCtx, q.rdb, []string{
		keyWork(currentWorkID),
		keyLock(currentWork.SessionID),
		keyQueue(currentWork.SessionID),
		keyActive(currentWork.SessionID),
	}, currentWorkID, workerID, credential, now, DefaultLockTTLSeconds).Result()
	if errors.Is(err, redis.Nil) {
		span.SetAttributes(attribute.Bool("queue.next_claimed", false))
		return nil, nil
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("rtcqueue: complete and claim next: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		span.SetAttributes(attribute.Bool("queue.next_claimed", false))
		return nil, nil
	}
	nextWorkID, ok := arr[0].(string)
	if !ok {
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected work_id type %T", arr[0]))
		return nil, fmt.Errorf("rtcqueue: complete and claim: unexpected work_id type %T", arr[0])
	}
	cred, ok := arr[1].(string)
	if !ok {
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected credential type %T", arr[1]))
		return nil, fmt.Errorf("rtcqueue: complete and claim: unexpected credential type %T", arr[1])
	}
	if nextWorkID == "" {
		span.SetAttributes(attribute.Bool("queue.next_claimed", false))
		return nil, nil
	}
	span.SetAttributes(
		attribute.Bool("queue.next_claimed", true),
		attribute.String("work.next_id", nextWorkID),
	)
	return &ClaimResult{
		SessionID:  currentWork.SessionID,
		WorkID:     nextWorkID,
		Credential: cred,
	}, nil
}

// ReleaseSession atomically releases the session lock and active-work pointer
// only if the caller holds the correct worker_id + credential. This prevents
// a race where one worker's ReleaseSession deletes another worker's lock
// after a lock handoff.
//
// Returns (true, nil) if the lock was successfully released.
// Returns (false, nil) if the lock is missing or owned by someone else.
// Returns (false, err) on Redis errors.
func (q *Queue) ReleaseSession(ctx context.Context, sessionID, workerID, credential string) (bool, error) {
	ctx, span := queueTracer().Start(ctx, "queue.release_session",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("worker.id", workerID),
		),
	)
	defer span.End()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	releaseCtx, cancel := context.WithTimeout(ctx, ReleaseTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the script is idempotent: if the lock is
	// already released or owned by someone else, it will return 0.
	var result int
	_, err := WithRetry(releaseCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		result, runErr = releaseSessionWithCredentialScript.Run(ctx, q.rdb, []string{
			keyLock(sessionID),
			keyActive(sessionID),
		}, workerID, credential).Int()
		return struct{}{}, runErr
	})

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("rtcqueue: release session: %w", err)
	}

	released := result == 1
	span.SetAttributes(attribute.Bool("queue.released", released))
	return released, nil
}

// ForceReleaseSession unconditionally drops the session lock and the
// active-work pointer. Used ONLY during recovery scenarios (e.g., service
// startup when recovering stale turns) where no credential is available.
//
// WARNING: This method can cause split-brain if called while a worker
// legitimately holds the lock. Use ReleaseSession for normal operation.
func (q *Queue) ForceReleaseSession(ctx context.Context, sessionID string) error {
	ctx, span := queueTracer().Start(ctx, "queue.force_release_session",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("release.mode", "force"),
		),
	)
	defer span.End()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	forceCtx, cancel := context.WithTimeout(ctx, ReleaseTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because DEL is idempotent: deleting non-existent keys
	// returns 0, which is acceptable for a force release.
	_, err := WithRetry(forceCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, q.rdb.Del(ctx, keyLock(sessionID), keyActive(sessionID)).Err()
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// HasPendingWorkByKind returns true if the session's queue contains a pending
// work item whose Data field, when JSON-decoded, has the given WorkKind.
// It also checks the currently-active work item (if any) for the same session.
//
// This is used for dedup: e.g., before publishing a compact work item, the
// caller checks whether a compact is already pending or processing.
//
// Implemented as a single Lua script to avoid N+1 Redis round-trips
// (ZRange + per-item HGet) and ensure atomicity.
func (q *Queue) HasPendingWorkByKind(ctx context.Context, sessionID string, kind string) (bool, error) {
	ctx, span := queueTracer().Start(ctx, "queue.has_pending_work_by_kind",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("work.kind", kind),
		),
	)
	defer span.End()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	checkCtx, cancel := context.WithTimeout(ctx, DefaultRedisTimeout)
	defer cancel()

	n, err := hasPendingWorkByKindScript.Run(checkCtx, q.rdb, []string{
		keyQueue(sessionID),
		keyActive(sessionID),
	}, kind).Int()
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("rtcqueue: has pending work by kind: %w", err)
	}
	span.SetAttributes(attribute.Bool("queue.has_pending", n == 1))
	return n == 1, nil
}

// SubscribeNew returns a Pub/Sub subscribed to the session:new channel.
// Callers are responsible for closing it.
func (q *Queue) SubscribeNew(ctx context.Context) *redis.PubSub {
	_, span := queueTracer().Start(ctx, "queue.subscribe_new")
	defer span.End()
	return q.rdb.Subscribe(ctx, ChannelSessionNew)
}

// SubscribeCancel returns a Pub/Sub subscribed to the cancel channel of
// a specific session.
func (q *Queue) SubscribeCancel(ctx context.Context, sessionID string) *redis.PubSub {
	_, span := queueTracer().Start(ctx, "queue.subscribe_cancel",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
		),
	)
	defer span.End()
	return q.rdb.Subscribe(ctx, ChannelSessionCancel(sessionID))
}

// generateCredential generates a new UUID v7 credential.
func generateCredential() string {
	return uuid.Must(uuid.NewV7()).String()
}
