package webfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// LLMResultCache caches LLM extraction results to avoid redundant LLM calls.
// Cache key is based on content hash + prompt hash.
type LLMResultCache struct {
	redisClient redis.UniversalClient
	ttl         time.Duration
}

// NewLLMResultCache creates a new LLM result cache.
func NewLLMResultCache(redisClient redis.UniversalClient, ttl time.Duration) *LLMResultCache {
	if ttl <= 0 {
		ttl = 1 * time.Hour // Default 1 hour
	}
	return &LLMResultCache{
		redisClient: redisClient,
		ttl:         ttl,
	}
}

// buildCacheKey creates a cache key from content and prompt.
// Uses SHA256 hash to keep keys short and avoid storing large content in keys.
func (c *LLMResultCache) buildCacheKey(content, prompt string) string {
	contentHash := sha256.Sum256([]byte(content))
	promptHash := sha256.Sum256([]byte(prompt))
	return fmt.Sprintf("webfetch:llm:%s:%s",
		hex.EncodeToString(contentHash[:8]),
		hex.EncodeToString(promptHash[:8]))
}

// Get retrieves a cached LLM result.
// Returns the cached result and true if found, empty string and false otherwise.
func (c *LLMResultCache) Get(ctx context.Context, content, prompt string) (string, bool) {
	if c == nil || c.redisClient == nil {
		return "", false
	}

	key := c.buildCacheKey(content, prompt)
	result, err := c.redisClient.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}

	return result, true
}

// Set stores an LLM result in the cache.
func (c *LLMResultCache) Set(ctx context.Context, content, prompt, result string) {
	if c == nil || c.redisClient == nil {
		return
	}

	key := c.buildCacheKey(content, prompt)
	c.redisClient.Set(ctx, key, result, c.ttl)
}

// Delete removes a cached LLM result.
func (c *LLMResultCache) Delete(ctx context.Context, content, prompt string) {
	if c == nil || c.redisClient == nil {
		return
	}

	key := c.buildCacheKey(content, prompt)
	c.redisClient.Del(ctx, key)
}
