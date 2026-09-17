// Package oauth provides OAuth2 state (CSRF protection) storage implementation.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rtc-agent/server/internal/infra/cache"

	"github.com/redis/go-redis/v9"
)

// ErrStateNotFound indicates the state does not exist or has expired.
var ErrStateNotFound = errors.New("state not found")

// RedisStore is a Redis-backed StateStore implementation for production and
// distributed environments.
//
// Storage layout:
//
//	key   = oauth2:state:{state}   (constructed via cache.OAuth2State)
//	value = provider name
//	TTL   = caller-specified (typically 10 minutes)
//
// GetDel uses a Lua script for atomic read-and-delete to prevent replay attacks.
type RedisStore struct {
	client redis.UniversalClient
}

// NewRedisStore creates a new Redis-backed state store.
func NewRedisStore(client redis.UniversalClient) *RedisStore {
	if client == nil {
		panic("statestore: redis client is nil")
	}
	return &RedisStore{client: client}
}

// Set stores a state value with the given TTL.
func (s *RedisStore) Set(ctx context.Context, state string, value string, ttl time.Duration) error {
	if state == "" {
		return errors.New("state key is empty")
	}
	key := cache.OAuth2State(state)
	return s.client.Set(ctx, key, value, ttl).Err()
}

// GetDel atomically retrieves and deletes a state value.
// Returns ErrStateNotFound if the state does not exist or has expired.
func (s *RedisStore) GetDel(ctx context.Context, state string) (string, error) {
	if state == "" {
		return "", ErrStateNotFound
	}
	key := cache.OAuth2State(state)

	result, err := cache.GetDel.Run(ctx, s.client, []string{key}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrStateNotFound
		}
		return "", fmt.Errorf("redis GetDel state: %w", err)
	}

	str, ok := result.(string)
	if !ok {
		return "", ErrStateNotFound
	}
	return str, nil
}
