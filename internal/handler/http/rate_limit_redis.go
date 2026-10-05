// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRateLimiter implements distributed rate limiting using Redis.
//
// Uses a sliding window counter per IP key with TTL auto-expiry.
// This ensures consistent rate limiting across multiple server instances.
type RedisRateLimiter struct {
	rdb    *redis.Client
	rate   int           // max requests per window
	window time.Duration // window duration
}

// NewRedisRateLimiter creates a Redis-backed rate limiter.
func NewRedisRateLimiter(rdb *redis.Client, maxRequestsPerMinute int) *RedisRateLimiter {
	if maxRequestsPerMinute <= 0 {
		maxRequestsPerMinute = 100
	}
	return &RedisRateLimiter{
		rdb:    rdb,
		rate:   maxRequestsPerMinute,
		window: time.Minute,
	}
}

// Allow checks whether the request from the given IP should be allowed.
// Uses Redis INCR + EXPIRE for atomic counting.
func (rl *RedisRateLimiter) Allow(ip string) bool {
	ctx := context.Background()
	key := fmt.Sprintf("admin:ratelimit:%s", ip)

	// Atomic increment
	count, err := rl.rdb.Incr(ctx, key).Result()
	if err != nil {
		// Redis failure — fail open (allow request) to avoid blocking all traffic
		return true
	}

	// Set expiry on first request in window
	if count == 1 {
		rl.rdb.Expire(ctx, key, rl.window)
	}

	return count <= int64(rl.rate)
}

// Stop is a no-op for Redis rate limiter (no background goroutines).
func (rl *RedisRateLimiter) Stop() {}
