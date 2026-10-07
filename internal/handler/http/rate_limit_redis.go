// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// rateLimitScript atomically increments a counter and sets its TTL on first increment.
// This prevents the race condition where INCR succeeds but EXPIRE is lost if the
// process crashes between the two commands.
var rateLimitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

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
// Uses a Lua script for atomic INCR + EXPIRE to prevent race conditions.
func (rl *RedisRateLimiter) Allow(ip string) bool {
	// Use a timeout context to avoid blocking indefinitely on Redis issues.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := fmt.Sprintf("admin:ratelimit:%s", ip)

	count, err := rateLimitScript.Run(ctx, rl.rdb, []string{key}, int(rl.window.Seconds())).Int()
	if err != nil {
		// Redis failure — fail open (allow request) to avoid blocking all traffic
		return true
	}

	return count <= rl.rate
}

// Stop is a no-op for Redis rate limiter (no background goroutines).
func (rl *RedisRateLimiter) Stop() {}
