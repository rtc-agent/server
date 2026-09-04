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

// ReleaseSession drops the session lock and the active-work pointer
// unconditionally. Used during graceful shutdown.
func (q *Queue) ReleaseSession(ctx context.Context, sessionID string) error {
	return q.rdb.Del(ctx, keyLock(sessionID), keyActive(sessionID)).Err()
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
