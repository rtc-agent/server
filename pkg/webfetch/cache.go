package webfetch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// FetchCache is a Redis-backed distributed cache for FetchResponse.
type FetchCache struct {
	client redis.UniversalClient
	prefix string
}

// NewFetchCache creates a FetchCache.
func NewFetchCache(client redis.UniversalClient) *FetchCache {
	return &FetchCache{
		client: client,
		prefix: "webfetch:",
	}
}

// Get retrieves a cached FetchResponse. Returns (nil, false) on miss or error.
func (c *FetchCache) Get(ctx context.Context, key string) (*FetchResponse, bool) {
	if c.client == nil {
		return nil, false
	}
	data, err := c.client.Get(ctx, c.prefix+key).Result()
	if err != nil {
		return nil, false
	}
	var resp FetchResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		return nil, false
	}
	return &resp, true
}

// Set writes a FetchResponse to the cache with the given TTL.
// Errors are silently ignored (cache is optional).
func (c *FetchCache) Set(ctx context.Context, key string, resp *FetchResponse, ttl time.Duration) {
	if c.client == nil {
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	c.client.Set(ctx, c.prefix+key, data, ttl)
}

// Ping checks Redis connectivity (used by HealthCheck).
func (c *FetchCache) Ping(ctx context.Context) error {
	if c.client == nil {
		return nil
	}
	return c.client.Ping(ctx).Err()
}

// buildCacheKey produces a deterministic cache key from URL + prompt.
// Uses a length-prefixed format to avoid collisions when URL contains "|".
func (m *WebFetchManager) buildCacheKey(rawURL, prompt string) string {
	raw := fmt.Sprintf("%d:%s|%s", len(rawURL), rawURL, prompt)
	hash := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", hash[:16])
}
