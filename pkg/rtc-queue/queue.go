// Package rtcqueue is a thin, Redis-backed distributed queue. It exposes
// only the primitives needed to publish, claim, complete, and cancel Work
// items scoped to a Session. Every write goes through a Lua script so
// each operation is atomic. The package intentionally contains no
// scheduling loop or worker goroutines — callers compose these
// primitives to build their own lifecycle.
package rtcqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrAlreadyTerminal is returned when Cancel is invoked on a Work item
// whose status is already completed or cancelled.
var ErrAlreadyTerminal = fmt.Errorf("rtcqueue: work already completed or cancelled")

// Queue is the entry point to the distributed queue. A Queue is safe for
// concurrent use and is expected to be shared across an application.

type Queue struct {
	rdb *redis.Client
}

// New constructs a Queue backed by the given Redis client.
func New(rdb *redis.Client) *Queue {
	return &Queue{rdb: rdb}
}

// Client returns the underlying redis.Client, primarily for tests.
func (q *Queue) Client() *redis.Client { return q.rdb }

// Publish enqueues a new Work item. The item is persisted, appended to
// the session's priority queue, and a notification is broadcast on the
// session:new channel so idle workers can wake up and try to claim it.
// All three writes are performed atomically by a single Lua script.
func (q *Queue) Publish(ctx context.Context, sessionID, data string, priority int64) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("rtcqueue: session_id required")
	}

	workID := uuid.New().String()
	now := time.Now().Unix()

	workKey := keyWork(workID)
	queueKey := keyQueue(sessionID)

	if err := publishScript.Run(ctx, q.rdb, []string{workKey, queueKey},
		workID, sessionID, data, priority, now, now,
		ChannelSessionNew, sessionID,
	).Err(); err != nil {
		return "", fmt.Errorf("rtcqueue: publish: %w", err)
	}
	return workID, nil
}

// Claim attempts to atomically claim the next pending Work item for the
// given session on behalf of workerID. If the session is already locked
// by another worker, or its queue is empty, Claim returns nil with a
// nil error.
func (q *Queue) Claim(ctx context.Context, sessionID, workerID string) (*ClaimResult, error) {
	if sessionID == "" || workerID == "" {
		return nil, fmt.Errorf("rtcqueue: session_id and worker_id required")
	}

	now := time.Now().Unix()
	res, err := claimScript.Run(ctx, q.rdb, []string{
		keyLock(sessionID),
		keyQueue(sessionID),
		keyActive(sessionID),
	}, workerID, DefaultLockTTLSeconds, now).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rtcqueue: claim: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, nil
	}
	workID, ok := arr[0].(string)
	if !ok {
		return nil, fmt.Errorf("rtcqueue: claim: unexpected work_id type %T", arr[0])
	}
	return &ClaimResult{SessionID: sessionID, WorkID: workID}, nil
}

// ClaimWithCredential claims a work item with credential-based lock ownership.
// This enables "hold lock" mode where a worker can continuously claim work items
// for a session without releasing the lock between items.
//
// First claim: pass an empty credential. The method generates a new credential
// (UUID) and returns it in ClaimResult. The worker MUST save this credential.
//
// Subsequent claims: pass the credential from the first claim. If the credential
// matches the lock owner, the claim succeeds. Otherwise, returns nil (access denied).
//
// This design ensures that only the worker who first claimed the session can
// continue processing its work items, preventing other workers from interfering.
func (q *Queue) ClaimWithCredential(ctx context.Context, sessionID, workerID, credential string) (*ClaimResult, error) {
	if sessionID == "" || workerID == "" {
		return nil, fmt.Errorf("rtcqueue: session_id and worker_id required")
	}

	// Generate credential if not provided (first claim)
	if credential == "" {
		credential = uuid.New().String()
	}

	now := time.Now().Unix()
	res, err := claimWithCredentialScript.Run(ctx, q.rdb, []string{
		keyLock(sessionID),
		keyQueue(sessionID),
		keyActive(sessionID),
	}, workerID, credential, DefaultLockTTLSeconds, now).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rtcqueue: claim with credential: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		return nil, nil
	}
	workID, ok := arr[0].(string)
	if !ok {
		return nil, fmt.Errorf("rtcqueue: claim: unexpected work_id type %T", arr[0])
	}
	cred, ok := arr[1].(string)
	if !ok {
		return nil, fmt.Errorf("rtcqueue: claim: unexpected credential type %T", arr[1])
	}
	return &ClaimResult{
		SessionID:  sessionID,
		WorkID:     workID,
		Credential: cred,
	}, nil
}

// LoadWork fetches a Work item by id. Returns nil, nil when the key is
// absent.
func (q *Queue) LoadWork(ctx context.Context, workID string) (*Work, error) {
	fields, err := q.rdb.HGetAll(ctx, keyWork(workID)).Result()
	if err != nil {
		return nil, fmt.Errorf("rtcqueue: load work: %w", err)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return workFromFields(fields)
}

// Complete marks a Work item as completed and releases the session lock.
// Both writes happen in a single Lua script.
//
// Ownership note: the session lock is derived from the work item's
// stored session_id, not from any caller-supplied argument. Callers
// MUST only invoke Complete on work items they themselves claimed —
// completing someone else's work will yank the lock out from under the
// processing worker. This contract is not enforced server-side.
func (q *Queue) Complete(ctx context.Context, workID string) error {
	n, err := completeScript.Run(ctx, q.rdb, []string{keyWork(workID)},
		time.Now().Unix(),
		ChannelSessionNew,
	).Int()
	if err != nil {
		return fmt.Errorf("rtcqueue: complete: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
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
	// First, load the work to get sessionID
	work, err := q.LoadWork(ctx, workID)
	if err != nil {
		return fmt.Errorf("rtcqueue: complete work: load work: %w", err)
	}
	if work == nil {
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}

	n, err := completeWorkScript.Run(ctx, q.rdb, []string{
		keyWork(workID),
		keyActive(work.SessionID),
	}, time.Now().Unix()).Int()
	if err != nil {
		return fmt.Errorf("rtcqueue: complete work: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
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
	msg := CancelMessage{
		WorkID:    workID,
		Reason:    reason,
		Timestamp: time.Now().Unix(),
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("rtcqueue: marshal cancel: %w", err)
	}

	n, err := cancelScript.Run(ctx, q.rdb, []string{keyWork(workID)},
		msg.Timestamp,
		string(payload),
	).Int()
	if err != nil {
		return fmt.Errorf("rtcqueue: cancel: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}
	if n < 0 {
		return ErrAlreadyTerminal
	}
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
	if sessionID == "" {
		return fmt.Errorf("rtcqueue: session_id required")
	}

	now := time.Now().Unix()

	// 1. Atomically remove all pending work items from the session queue and
	// mark them as cancelled.
	_, err := cancelSessionScript.Run(ctx, q.rdb, []string{keyQueue(sessionID)},
		now,
	).Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("rtcqueue: cancel session pending: %w", err)
	}

	// 2. Cancel the work item currently being processed (if any). The
	// cancelSessionActiveScript reads the session:active pointer, publishes
	// a cancel notification, and releases both the lock and the pointer —
	// all atomically. If no work is active the script returns nil and we
	// are done.
	_, err = cancelSessionActiveScript.Run(ctx, q.rdb, []string{
		keyLock(sessionID),
		keyActive(sessionID),
		ChannelSessionCancel(sessionID),
	}, reason, now).Result()
	if err != nil && err != redis.Nil {
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
	n, err := renewLockScript.Run(ctx, q.rdb, []string{keyLock(sessionID)},
		workerID, DefaultLockTTLSeconds,
	).Int()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rtcqueue: renew lock: %w", err)
	}
	return n == 1, nil
}

// RenewLockWithCredential refreshes the session lock TTL atomically for
// hash-based locks used in "hold lock" mode. Returns false if the lock
// is no longer held by this worker with the correct credential.
func (q *Queue) RenewLockWithCredential(ctx context.Context, sessionID, workerID, credential string) (bool, error) {
	n, err := renewLockWithCredentialScript.Run(ctx, q.rdb, []string{keyLock(sessionID)},
		workerID, credential, DefaultLockTTLSeconds,
	).Int()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rtcqueue: renew lock with credential: %w", err)
	}
	return n == 1, nil
}

// CompleteAndClaimNext atomically completes the current work item and claims
// the next work item from the session queue if available. This enables "hold
// lock" mode where a worker can process multiple work items without releasing
// and re-acquiring the session lock.
//
// Returns (nil, nil) if there is no next work item in the queue.
// Returns (*ClaimResult, nil) if the next work was successfully claimed.
// Returns (nil, error) on Redis errors.
func (q *Queue) CompleteAndClaimNext(ctx context.Context, workID, sessionID, workerID string, credential string) (*ClaimResult, error) {
	now := time.Now().Unix()
	result, err := completeAndClaimNextScript.Run(ctx, q.rdb, []string{
		keyWork(workID),
		keyQueue(sessionID),
		keyActive(sessionID),
		keyLock(sessionID),
	}, now, workerID, DefaultLockTTLSeconds).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rtcqueue: complete and claim next: %w", err)
	}
	arr, ok := result.([]interface{})
	if !ok || len(arr) < 2 {
		return nil, nil
	}
	nextWorkID, _ := arr[0].(string)
	newCred, _ := arr[1].(string)
	if nextWorkID == "" {
		return nil, nil
	}
	return &ClaimResult{
		SessionID:  sessionID,
		WorkID:     nextWorkID,
		Credential: newCred,
	}, nil
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
	// Load current work to get sessionID
	currentWork, err := q.LoadWork(ctx, currentWorkID)
	if err != nil {
		return nil, fmt.Errorf("rtcqueue: complete and claim: load work: %w", err)
	}
	if currentWork == nil {
		return nil, fmt.Errorf("rtcqueue: work %s not found", currentWorkID)
	}

	now := time.Now().Unix()
	res, err := completeWorkAndClaimNextScript.Run(ctx, q.rdb, []string{
		keyWork(currentWorkID),
		keyLock(currentWork.SessionID),
		keyQueue(currentWork.SessionID),
		keyActive(currentWork.SessionID),
	}, currentWorkID, workerID, credential, now, DefaultLockTTLSeconds).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rtcqueue: complete and claim next: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		return nil, nil
	}
	nextWorkID, ok := arr[0].(string)
	if !ok {
		return nil, fmt.Errorf("rtcqueue: complete and claim: unexpected work_id type %T", arr[0])
	}
	cred, ok := arr[1].(string)
	if !ok {
		return nil, fmt.Errorf("rtcqueue: complete and claim: unexpected credential type %T", arr[1])
	}
	if nextWorkID == "" {
		return nil, nil
	}
	return &ClaimResult{
		SessionID:  currentWork.SessionID,
		WorkID:     nextWorkID,
		Credential: cred,
	}, nil
}

// ReleaseSession drops the session lock and the active-work pointer
// unconditionally. Used during graceful shutdown.
func (q *Queue) ReleaseSession(ctx context.Context, sessionID string) error {
	return q.rdb.Del(ctx, keyLock(sessionID), keyActive(sessionID)).Err()
}

// RequeueGhostWork checks if a session has a ghost work item (processing but
// lock expired) and requeues it atomically via Lua script. Returns the work ID
// if requeued, empty string if no ghost work found.
//
// A ghost work occurs when a worker crashes after claiming a work item but
// before completing it. The work remains in "processing" state while the
// session lock expires (TTL 120s, renewal 30s). This method detects such
// orphaned work and moves it back to "pending" for another worker to claim.
//
// The check is safe: if the lock still exists, the worker is likely alive
// and we should not interfere.
func (q *Queue) RequeueGhostWork(ctx context.Context, sessionID string) (string, error) {
	now := time.Now().Unix()
	res, err := requeueGhostWorkScript.Run(ctx, q.rdb, []string{
		keyLock(sessionID),
		keyActive(sessionID),
		keyQueue(sessionID),
	}, now).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("rtcqueue: requeue ghost work: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) == 0 {
		return "", nil
	}
	workID, ok := arr[0].(string)
	if !ok {
		return "", nil
	}
	return workID, nil
}

// RequeueGhostWorksBatch requeues ghost work items for multiple sessions in
// a single Redis pipeline. Returns a map of sessionID → requeued workID.
//
// This is more efficient than calling RequeueGhostWork in a loop when
// recovering many sessions at once (e.g., during server restart).
func (q *Queue) RequeueGhostWorksBatch(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}

	now := time.Now().Unix()
	pipe := q.rdb.Pipeline()
	cmds := make([]*redis.Cmd, len(sessionIDs))

	// Queue all Lua script calls
	for i, sid := range sessionIDs {
		cmds[i] = requeueGhostWorkScript.Run(ctx, pipe, []string{
			keyLock(sid),
			keyActive(sid),
			keyQueue(sid),
		}, now)
	}

	// Execute pipeline
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("rtcqueue: batch requeue ghost works: %w", err)
	}

	// Parse results
	result := make(map[string]string)
	for i, cmd := range cmds {
		res, err := cmd.Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			continue // Skip failed sessions
		}
		arr, ok := res.([]interface{})
		if !ok || len(arr) == 0 {
			continue
		}
		workID, ok := arr[0].(string)
		if ok && workID != "" {
			result[sessionIDs[i]] = workID
		}
	}

	return result, nil
}

// RequeueWork atomically moves a work item from "processing" back to "pending"
// and re-adds it to the session's priority queue. This prevents "ghost work" —
// items claimed from Redis but never processed because the target TurnLoop had
// already stopped (e.g., Push failed after ClaimWithCredential succeeded).
//
// Returns nil on success. Returns an error if the work is not found or not in
// "processing" state.
func (q *Queue) RequeueWork(ctx context.Context, workID string) error {
	work, err := q.LoadWork(ctx, workID)
	if err != nil {
		return fmt.Errorf("rtcqueue: requeue work: load work: %w", err)
	}
	if work == nil {
		return fmt.Errorf("rtcqueue: work %s not found", workID)
	}

	now := time.Now().Unix()
	n, err := requeueWorkScript.Run(ctx, q.rdb, []string{
		keyWork(workID),
		keyQueue(work.SessionID),
	}, now).Int()
	if err != nil {
		return fmt.Errorf("rtcqueue: requeue work: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("rtcqueue: work %s not in processing state", workID)
	}
	return nil
}

// HasPendingWorkByKind returns true if the session's queue contains a pending
// work item whose Data field, when JSON-decoded, has the given WorkKind.
// It also checks the currently-active work item (if any) for the same session.
//
// This is used for dedup: e.g., before publishing a compact work item, the
// caller checks whether a compact is already pending or processing.
func (q *Queue) HasPendingWorkByKind(ctx context.Context, sessionID string, kind string) (bool, error) {
	// 1. Check pending items in the session queue (zset members = work IDs).
	workIDs, err := q.rdb.ZRange(ctx, keyQueue(sessionID), 0, -1).Result()
	if err != nil {
		return false, fmt.Errorf("rtcqueue: zrange queue: %w", err)
	}
	for _, wid := range workIDs {
		data, err := q.rdb.HGet(ctx, keyWork(wid), "data").Result()
		if err != nil {
			continue
		}
		var p struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(data), &p) == nil && p.Kind == kind {
			return true, nil
		}
	}

	// 2. Check the active (processing) work item, if any.
	activeID, err := q.rdb.Get(ctx, keyActive(sessionID)).Result()
	if err == nil && activeID != "" {
		data, err := q.rdb.HGet(ctx, keyWork(activeID), "data").Result()
		if err == nil {
			var p struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal([]byte(data), &p) == nil && p.Kind == kind {
				return true, nil
			}
		}
	}

	return false, nil
}

// SubscribeNew returns a Pub/Sub subscribed to the session:new channel.
// Callers are responsible for closing it.
func (q *Queue) SubscribeNew(ctx context.Context) *redis.PubSub {
	return q.rdb.Subscribe(ctx, ChannelSessionNew)
}

// SubscribeCancel returns a Pub/Sub subscribed to the cancel channel of
// a specific session.
func (q *Queue) SubscribeCancel(ctx context.Context, sessionID string) *redis.PubSub {
	return q.rdb.Subscribe(ctx, ChannelSessionCancel(sessionID))
}

// --- field marshaling ---------------------------------------------------

func workFromFields(m map[string]string) (*Work, error) {
	w := &Work{
		ID:        m["id"],
		SessionID: m["session_id"],
		Data:      m["data"],
		WorkerID:  m["worker_id"],
		Status:    WorkStatus(m["status"]),
	}
	parseRequired := func(key string) (int64, error) {
		v, err := strconv.ParseInt(m[key], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("rtcqueue: parse field %q=%q: %w", key, m[key], err)
		}
		return v, nil
	}
	var err error
	if w.Priority, err = parseRequired("priority"); err != nil {
		return nil, err
	}
	if v, err := parseRequired("created_at"); err != nil {
		return nil, err
	} else {
		w.CreatedAt = time.Unix(v, 0)
	}
	if v, err := parseRequired("updated_at"); err != nil {
		return nil, err
	} else {
		w.UpdatedAt = time.Unix(v, 0)
	}
	if claimedRaw, ok := m["claimed_at"]; ok && claimedRaw != "" && claimedRaw != "0" {
		if v, err := strconv.ParseInt(claimedRaw, 10, 64); err != nil {
			return nil, fmt.Errorf("rtcqueue: parse field \"claimed_at\"=%q: %w", claimedRaw, err)
		} else if v > 0 {
			w.ClaimedAt = time.Unix(v, 0)
		}
	}
	return w, nil
}
