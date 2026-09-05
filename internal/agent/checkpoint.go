package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"

	"github.com/cloudwego/eino/adk"
	"github.com/redis/go-redis/v9"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// redisCheckpointStore implements adk.CheckPointStore using Redis as the backend.
//
// It persists agent checkpoint data with a configurable TTL, enabling session
// recovery across process restarts and interrupt/resume functionality.
//
// This is a fresh implementation (not a copy) of the checkpoint store in
// internal/worker/checkpoint_store.go. The key pattern is the same
// (checkpoint:{id} via cache.Checkpoint), but the implementation is
// tailored for the new agent package.
//
// Error handling: redis.Nil is converted to (nil, false, nil) per the
// project's Redis error-handling convention.
type redisCheckpointStore struct {
	redis redis.UniversalClient
	ttl   time.Duration
}

// newRedisCheckpointStore creates a redisCheckpointStore with the given TTL.
// A TTL of 0 means no expiration (not recommended for production use).
//
// The recommended TTL is 24 hours — long enough to survive worker restarts
// and slow RTC result submissions, short enough to avoid unbounded Redis
// growth from abandoned checkpoints.
func newRedisCheckpointStore(rdb redis.UniversalClient, ttl time.Duration) *redisCheckpointStore {
	return &redisCheckpointStore{
		redis: rdb,
		ttl:   ttl,
	}
}

// Set persists checkpoint data to Redis with the configured TTL.
//
// Uses context.Background() for Redis operations to ensure checkpoint
// persistence is not affected by session or request context cancellation.
// A checkpoint save must survive even if the parent context is cancelled —
// otherwise the turn's state is lost and cannot be resumed.
func (s *redisCheckpointStore) Set(ctx context.Context, id string, data []byte) error {
	key := cache.Checkpoint(id)
	if err := s.redis.Set(context.Background(), key, data, s.ttl).Err(); err != nil {
		return fmt.Errorf("checkpoint set: id=%s key=%s: %w", id, key, err)
	}
	return nil
}

// Get retrieves checkpoint data from Redis.
//
// If the key does not exist, it returns (nil, false, nil) — not an error.
// This is the expected behavior when no checkpoint exists (e.g., on the
// first turn of a session).
//
// Uses context.Background() for the same reason as Set: checkpoint reads
// must not be affected by context cancellation.
func (s *redisCheckpointStore) Get(ctx context.Context, id string) ([]byte, bool, error) {
	key := cache.Checkpoint(id)
	data, err := s.redis.Get(context.Background(), key).Result()
	if errors.Is(err, redis.Nil) {
		// Key does not exist — return (nil, false, nil) per convention.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("checkpoint get: id=%s key=%s: %w", id, key, err)
	}
	return []byte(data), true, nil
}

// Delete removes checkpoint data from Redis.
//
// Called by eino's TurnLoop on clean exit (no checkpoint saved) to prevent
// stale resumption. Without this method, old checkpoints persist and cause
// subsequent turns to incorrectly take the GenResume path instead of GenInput.
//
// Uses context.Background() for consistency with Set/Get — checkpoint deletion
// must not be affected by context cancellation.
func (s *redisCheckpointStore) Delete(ctx context.Context, id string) error {
	key := cache.Checkpoint(id)
	if err := s.redis.Del(context.Background(), key).Err(); err != nil {
		return fmt.Errorf("checkpoint delete: id=%s key=%s: %w", id, key, err)
	}
	return nil
}

// compile-time check: redisCheckpointStore implements adk.CheckPointStore.
var _ adk.CheckPointStore = (*redisCheckpointStore)(nil)

// metricsCheckpointStore wraps an adk.CheckPointStore and records metrics for
// every Set/Get/Delete via turnagent.Metrics.RecordCheckpoint.
//
// This is the "instrumented decorator" pattern recommended by the Metrics
// interface docs: turn-agent owns the CheckPointStore lifecycle (via eino
// TurnLoop) but does not emit per-operation telemetry itself, so the
// application layer wraps the store to bridge the gap.
type metricsCheckpointStore struct {
	inner   adk.CheckPointStore
	metrics turnagent.Metrics
}

// newMetricsCheckpointStore wraps inner with metrics recording. If metrics is
// nil the wrapper degrades to a passthrough (no metrics, no overhead beyond
// the extra method call).
func newMetricsCheckpointStore(inner adk.CheckPointStore, metrics turnagent.Metrics) adk.CheckPointStore {
	if metrics == nil {
		return inner
	}
	return &metricsCheckpointStore{inner: inner, metrics: metrics}
}

func (s *metricsCheckpointStore) Set(ctx context.Context, id string, data []byte) error {
	start := time.Now()
	err := s.inner.Set(ctx, id, data)
	s.metrics.RecordCheckpoint(ctx, turnagent.CheckpointMetricsAttrs{
		SessionID:  turnagent.SessionIDFromContext(ctx),
		Operation:  "save",
		DataSize:   len(data),
		DurationMs: time.Since(start).Milliseconds(),
		Error:      err,
	})
	return err
}

func (s *metricsCheckpointStore) Get(ctx context.Context, id string) ([]byte, bool, error) {
	start := time.Now()
	data, found, err := s.inner.Get(ctx, id)
	s.metrics.RecordCheckpoint(ctx, turnagent.CheckpointMetricsAttrs{
		SessionID:  turnagent.SessionIDFromContext(ctx),
		Operation:  "load",
		DataSize:   len(data),
		DurationMs: time.Since(start).Milliseconds(),
		Error:      err,
	})
	return data, found, err
}

func (s *metricsCheckpointStore) Delete(ctx context.Context, id string) error {
	start := time.Now()
	var err error
	// adk.CheckPointStore only requires Get/Set; Delete lives on the optional
	// adk.CheckPointDeleter interface. If the inner store doesn't implement it,
	// deletion is a no-op (the inner relies on TTL for cleanup).
	if deleter, ok := s.inner.(adk.CheckPointDeleter); ok {
		err = deleter.Delete(ctx, id)
	}
	s.metrics.RecordCheckpoint(ctx, turnagent.CheckpointMetricsAttrs{
		SessionID:  turnagent.SessionIDFromContext(ctx),
		Operation:  "delete",
		DurationMs: time.Since(start).Milliseconds(),
		Error:      err,
	})
	return err
}

// compile-time check: metricsCheckpointStore implements adk.CheckPointStore.
var _ adk.CheckPointStore = (*metricsCheckpointStore)(nil)

// compile-time check: metricsCheckpointStore implements adk.CheckPointDeleter.
// We always implement it; if the inner store doesn't support deletion, Delete
// is a no-op (see implementation above).
var _ adk.CheckPointDeleter = (*metricsCheckpointStore)(nil)
