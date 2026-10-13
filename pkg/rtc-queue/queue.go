// Package rtcqueue is a thin, Redis-backed distributed queue. It exposes
// only the primitives needed to publish, claim, complete, and cancel Work
// items scoped to a Session. Every write goes through a Lua script so
// each operation is atomic. The package intentionally contains no
// scheduling loop or worker goroutines — callers compose these
// primitives to build their own lifecycle.
package rtcqueue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Timeout constants for Redis operations.
// These timeouts prevent unresponsive Redis from blocking callers indefinitely.
const (
	// DefaultRedisTimeout is the default timeout for most Redis operations.
	// 5 seconds balances between allowing slow queries and preventing indefinite blocking.
	DefaultRedisTimeout = 5 * time.Second

	// PublishTimeout is the timeout for Publish/PublishWithID operations.
	// Slightly longer to account for Lua script execution.
	PublishTimeout = 5 * time.Second

	// ClaimTimeout is the timeout for Claim operations.
	ClaimTimeout = 5 * time.Second

	// CompleteTimeout is the timeout for Complete/CompleteWork operations.
	CompleteTimeout = 5 * time.Second

	// CancelTimeout is the timeout for Cancel/CancelSession operations.
	CancelTimeout = 5 * time.Second

	// RenewLockTimeout is the timeout for RenewLock operations.
	RenewLockTimeout = 5 * time.Second

	// ReleaseTimeout is the timeout for ReleaseSession/ForceReleaseSession operations.
	ReleaseTimeout = 5 * time.Second
)

// ErrAlreadyTerminal is returned when Cancel is invoked on a Work item
// whose status is already completed or cancelled.
var ErrAlreadyTerminal = fmt.Errorf("rtcqueue: work already completed or cancelled")

// Queue is the entry point to the distributed queue. A Queue is safe for
// concurrent use and is expected to be shared across an application.
// It provides atomic publish, claim, complete, and cancel operations
// backed by Redis Lua scripts.
type Queue struct {
	rdb         *redis.Client
	retryConfig RetryConfig
}

// queueTracer returns the tracer for queue operations.
func queueTracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("rtc-queue")
}

// New constructs a Queue backed by the given Redis client.
// Uses default retry configuration for transient error recovery.
func New(rdb *redis.Client) *Queue {
	return NewWithConfig(rdb, DefaultRetryConfig())
}

// NewWithConfig constructs a Queue with a custom retry configuration.
// This allows callers to tune retry behavior for their specific environment.
func NewWithConfig(rdb *redis.Client, retryConfig RetryConfig) *Queue {
	return &Queue{rdb: rdb, retryConfig: retryConfig}
}

// Client returns the underlying redis.Client, primarily for tests.
func (q *Queue) Client() *redis.Client { return q.rdb }

// Publish enqueues a new Work item. The item is persisted, appended to
// the session's priority queue, and a notification is broadcast on the
// session:new channel so idle workers can wake up and try to claim it.
// All three writes are performed atomically by a single Lua script.
//
// Deprecated: Use PublishWithID instead for idempotent publishing.
// Publish generates a random UUID v7 work ID, which means retries will
// create duplicate work items. PublishWithID allows callers to specify
// a deterministic work ID (e.g., using GenerateDeterministicWorkID) to
// ensure exactly-once semantics even with retries.
//
// Example migration:
//
//	// Before:
//	workID, err := queue.Publish(ctx, sessionID, data, priority)
//
//	// After:
//	workID := rtcqueue.GenerateDeterministicWorkID(sessionID, uniqueKey)
//	workID, err := queue.PublishWithID(ctx, sessionID, workID, data, priority)
func (q *Queue) Publish(ctx context.Context, sessionID, data string, priority int64) (string, error) {
	workID := uuid.Must(uuid.NewV7()).String()
	return q.PublishWithID(ctx, sessionID, workID, data, priority)
}

// PublishWithID enqueues a new Work item with a caller-specified work ID.
// This enables idempotent publishing: retries with the same work ID will
// be skipped (the Lua script checks for existing work items).
//
// Use this method when you need exactly-once semantics, such as:
//   - Retry scenarios where the same logical work should not be duplicated
//   - Deterministic work IDs based on business keys (e.g., sessionID + messageID)
//
// Returns the work ID and nil error on success. If a work item with the
// same ID already exists, it returns the existing work ID and nil error
// (idempotent behavior).
func (q *Queue) PublishWithID(ctx context.Context, sessionID, workID, data string, priority int64) (string, error) {
	ctx, span := queueTracer().Start(ctx, "queue.publish",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("work.id", workID),
			attribute.Int64("work.priority", priority),
		),
	)
	defer span.End()

	if sessionID == "" {
		span.SetStatus(codes.Error, "session_id required")
		return "", fmt.Errorf("rtcqueue: session_id required")
	}
	if workID == "" {
		span.SetStatus(codes.Error, "work_id required")
		return "", fmt.Errorf("rtcqueue: work_id required")
	}

	now := time.Now().Unix()

	workKey := keyWork(workID)
	queueKey := keyQueue(sessionID)

	// Bound the Redis round-trip to 5s so an unresponsive broker cannot
	// stall the caller's goroutine indefinitely.
	publishCtx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the Lua script is idempotent: if the work item
	// already exists with active status, it will be skipped.
	var result int
	_, err := WithRetry(publishCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		result, runErr = publishScript.Run(ctx, q.rdb, []string{workKey, queueKey},
			workID, sessionID, data, priority, now, now,
			ChannelSessionNew, sessionID,
		).Int()
		return struct{}{}, runErr
	})
	if err != nil {
		globalQueueMetrics.recordPublish("error")
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("rtcqueue: publish: %w", err)
	}

	// result=1 means published, result=0 means already exists (idempotent skip)
	if result == 0 {
		span.SetAttributes(attribute.Bool("work.idempotent_skip", true))
		globalQueueMetrics.recordPublish("idempotent_skip")
	} else {
		globalQueueMetrics.recordPublish("success")
	}

	return workID, nil
}

// Claim attempts to atomically claim the next pending Work item for the
// given session on behalf of workerID. If the session is already locked
// by another worker, or its queue is empty, Claim returns nil with a
// nil error.
func (q *Queue) Claim(ctx context.Context, sessionID, workerID string) (*ClaimResult, error) {
	ctx, span := queueTracer().Start(ctx, "queue.claim",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("worker.id", workerID),
		),
	)
	defer span.End()

	if sessionID == "" || workerID == "" {
		span.SetStatus(codes.Error, "session_id and worker_id required")
		return nil, fmt.Errorf("rtcqueue: session_id and worker_id required")
	}

	now := time.Now().Unix()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	claimCtx, cancel := context.WithTimeout(ctx, ClaimTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the Lua script is atomic: if another worker
	// claims first, the lock will exist and this call will return nil.
	var res interface{}
	_, err := WithRetry(claimCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		res, runErr = claimScript.Run(ctx, q.rdb, []string{
			keyLock(sessionID),
			keyQueue(sessionID),
			keyActive(sessionID),
		}, workerID, DefaultLockTTLSeconds, now).Result()
		return struct{}{}, runErr
	})
	if errors.Is(err, redis.Nil) {
		span.SetAttributes(attribute.Bool("queue.claimed", false))
		globalQueueMetrics.recordClaim("empty")
		return nil, nil
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		globalQueueMetrics.recordClaim("error")
		return nil, fmt.Errorf("rtcqueue: claim: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) == 0 {
		span.SetAttributes(attribute.Bool("queue.claimed", false))
		globalQueueMetrics.recordClaim("empty")
		return nil, nil
	}
	workID, ok := arr[0].(string)
	if !ok {
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected work_id type %T", arr[0]))
		globalQueueMetrics.recordClaim("error")
		return nil, fmt.Errorf("rtcqueue: claim: unexpected work_id type %T", arr[0])
	}
	span.SetAttributes(
		attribute.Bool("queue.claimed", true),
		attribute.String("work.id", workID),
	)
	globalQueueMetrics.recordClaim("success")
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
	ctx, span := queueTracer().Start(ctx, "queue.claim_with_credential",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("worker.id", workerID),
			attribute.Bool("queue.first_claim", credential == ""),
		),
	)
	defer span.End()

	if sessionID == "" || workerID == "" {
		span.SetStatus(codes.Error, "session_id and worker_id required")
		return nil, fmt.Errorf("rtcqueue: session_id and worker_id required")
	}

	// Generate credential if not provided (first claim)
	if credential == "" {
		credential = generateCredential()
	}

	now := time.Now().Unix()

	// Add timeout to prevent indefinite blocking if Redis is unresponsive.
	claimCtx, cancel := context.WithTimeout(ctx, ClaimTimeout)
	defer cancel()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because the Lua script is atomic: credential check
	// happens inside the script, and if another worker interferes,
	// the script will return nil.
	var res interface{}
	_, err := WithRetry(claimCtx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		res, runErr = claimWithCredentialScript.Run(ctx, q.rdb, []string{
			keyLock(sessionID),
			keyQueue(sessionID),
			keyActive(sessionID),
		}, workerID, credential, DefaultLockTTLSeconds, now).Result()
		return struct{}{}, runErr
	})
	if errors.Is(err, redis.Nil) {
		span.SetAttributes(attribute.Bool("queue.claimed", false))
		globalQueueMetrics.recordClaim("empty")
		return nil, nil
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		globalQueueMetrics.recordClaim("error")
		return nil, fmt.Errorf("rtcqueue: claim with credential: %w", err)
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		span.SetAttributes(attribute.Bool("queue.claimed", false))
		globalQueueMetrics.recordClaim("empty")
		return nil, nil
	}
	workID, ok := arr[0].(string)
	if !ok {
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected work_id type %T", arr[0]))
		globalQueueMetrics.recordClaim("error")
		return nil, fmt.Errorf("rtcqueue: claim: unexpected work_id type %T", arr[0])
	}
	cred, ok := arr[1].(string)
	if !ok {
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected credential type %T", arr[1]))
		globalQueueMetrics.recordClaim("error")
		return nil, fmt.Errorf("rtcqueue: claim: unexpected credential type %T", arr[1])
	}
	span.SetAttributes(
		attribute.Bool("queue.claimed", true),
		attribute.String("work.id", workID),
	)
	globalQueueMetrics.recordClaim("success")
	return &ClaimResult{
		SessionID:  sessionID,
		WorkID:     workID,
		Credential: cred,
	}, nil
}

// LoadWork fetches a Work item by id. Returns nil, nil when the key is
// absent.
func (q *Queue) LoadWork(ctx context.Context, workID string) (*Work, error) {
	ctx, span := queueTracer().Start(ctx, "queue.load_work",
		trace.WithAttributes(
			attribute.String("work.id", workID),
		),
	)
	defer span.End()

	// Wrap the Redis call with retry for transient errors.
	// This is safe because HGETALL is a read-only operation.
	var fields map[string]string
	_, err := WithRetry(ctx, q.retryConfig, func(ctx context.Context) (struct{}, error) {
		var runErr error
		fields, runErr = q.rdb.HGetAll(ctx, keyWork(workID)).Result()
		return struct{}{}, runErr
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("rtcqueue: load work: %w", err)
	}
	if len(fields) == 0 {
		span.SetAttributes(attribute.Bool("work.found", false))
		return nil, nil
	}
	span.SetAttributes(attribute.Bool("work.found", true))
	return workFromFields(fields)
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
