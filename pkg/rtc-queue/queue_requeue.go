package rtcqueue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RequeueGhostWork requeues a work item that was claimed but never completed.
//
// A "ghost work" occurs when a worker crashes or is killed after claiming
// a work item but before completing it. The work remains in "processing"
// state while the session lock expires (TTL 120s, renewal 30s). This method
// detects such orphaned work and moves it back to "pending" for another
// worker to claim.
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
	if errors.Is(err, redis.Nil) {
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
// a single Redis pipeline. Returns a map of sessionID -> requeued workID.
//
// This is more efficient than calling RequeueGhostWork in a loop when
// recovering many sessions at once (e.g., during server restart).
func (q *Queue) RequeueGhostWorksBatch(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}

	// Ensure the Lua script is loaded in Redis before executing the pipeline.
	// Pipeline uses EVALSHA which requires the script to be cached in Redis.
	// Without this, a Redis restart or SCRIPT FLUSH would cause NOSCRIPT errors.
	// Load() is idempotent and fast if the script is already cached.
	if err := requeueGhostWorkScript.Load(ctx, q.rdb).Err(); err != nil {
		return nil, fmt.Errorf("rtcqueue: load requeue script: %w", err)
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
		if errors.Is(err, redis.Nil) {
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
