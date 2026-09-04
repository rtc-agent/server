package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/rediskey"

	"github.com/cloudwego/eino/adk"
	"github.com/redis/go-redis/v9"
)

// redisCheckpointStore implements adk.CheckPointStore using Redis as the backend.
//
// It persists agent checkpoint data with a configurable TTL, enabling session
// recovery across process restarts and interrupt/resume functionality.
//
// This is a fresh implementation (not a copy) of the checkpoint store in
// internal/worker/checkpoint_store.go. The key pattern is the same
// (checkpoint:{id} via rediskey.Checkpoint), but the implementation is
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
	key := rediskey.Checkpoint(id)
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
	key := rediskey.Checkpoint(id)
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
	key := rediskey.Checkpoint(id)
	if err := s.redis.Del(context.Background(), key).Err(); err != nil {
		return fmt.Errorf("checkpoint delete: id=%s key=%s: %w", id, key, err)
	}
	return nil
}

// compile-time check: redisCheckpointStore implements adk.CheckPointStore.
var _ adk.CheckPointStore = (*redisCheckpointStore)(nil)
