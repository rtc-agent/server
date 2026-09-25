package websearch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script for atomic rate limiting (INCR + EXPIRE in one atomic operation)
// KEYS[1]: websearch:rl:{provider}:{timestamp}
// ARGV[1]: TTL in seconds
// Returns: new count after increment
// Note: Always check and repair TTL (not just on count==1) to defend against
// orphaned keys if the TTL is ever removed externally (e.g., PERSIST, Redis
// restart without AOF).
const rateLimitLua = `
local key = KEYS[1]
local ttl = tonumber(ARGV[1])
local count = redis.call('INCR', key)
if redis.call('TTL', key) < 0 then
    redis.call('EXPIRE', key, ttl)
end
return count
`

var rateLimitScript = redis.NewScript(rateLimitLua)

// DistributedRateLimiter implements rate limiting using Redis with atomic INCR+EXPIRE
type DistributedRateLimiter struct {
	providerName string
	redisClient  *redis.Client
	rate         float64 // requests per second
	burst        int     // max burst size
	windowSize   time.Duration
}

// NewDistributedRateLimiter creates a distributed rate limiter
func NewDistributedRateLimiter(
	providerName string,
	redisClient *redis.Client,
	rate float64,
	burst int,
) *DistributedRateLimiter {
	// Use 1-second window for rate limiting
	windowSize := time.Second

	return &DistributedRateLimiter{
		providerName: providerName,
		redisClient:  redisClient,
		rate:         rate,
		burst:        burst,
		windowSize:   windowSize,
	}
}

// AllowWithErr checks if a request is allowed (returns error for Redis failures)
func (d *DistributedRateLimiter) AllowWithErr(ctx context.Context) (bool, error) {
	// Generate key with current timestamp (1-second precision)
	now := time.Now().Unix()
	key := fmt.Sprintf("websearch:rl:%s:%d", d.providerName, now)

	// Atomic increment with TTL using Lua script
	// TTL is window size + 1 second buffer to ensure key expires after window
	ttl := int(d.windowSize.Seconds()) + 1
	count, err := rateLimitScript.Run(ctx, d.redisClient, []string{key}, ttl).Int64()
	if err != nil {
		return true, err // Return error for caller to handle degradation
	}

	// Check if within limit.
	// burst controls the maximum allowed requests per window;
	// fall back to ceil(rate) when burst is unset.
	limit := int64(d.burst)
	if limit <= 0 {
		limit = int64(d.rate)
		if limit <= 0 {
			limit = 1
		}
	}

	return count <= limit, nil
}

// HybridRateLimiter combines local and distributed rate limiters
type HybridRateLimiter struct {
	local          *RateLimiter
	remote         *DistributedRateLimiter
	redisClient    *redis.Client
	redisHealthy   atomic.Bool
	checkStop      chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	internalCtx    context.Context
	internalCancel context.CancelFunc
}

// NewHybridRateLimiter creates a hybrid rate limiter
func NewHybridRateLimiter(
	local *RateLimiter,
	remote *DistributedRateLimiter,
	redisClient *redis.Client,
) *HybridRateLimiter {
	internalCtx, internalCancel := context.WithCancel(context.Background())
	h := &HybridRateLimiter{
		local:          local,
		remote:         remote,
		redisClient:    redisClient,
		checkStop:      make(chan struct{}),
		internalCtx:    internalCtx,
		internalCancel: internalCancel,
	}

	// Initially assume Redis is available, start background health check
	h.redisHealthy.Store(true)
	h.wg.Add(1)
	go h.redisHealthLoop()

	return h
}

// Allow checks if a request is allowed
// Uses distributed limiter if Redis is healthy, otherwise falls back to local
func (h *HybridRateLimiter) Allow() bool {
	if h.redisHealthy.Load() && h.remote != nil {
		// Use timeout context to prevent hanging
		ctx, cancel := context.WithTimeout(h.internalCtx, 2*time.Second)
		defer cancel()

		allowed, err := h.remote.AllowWithErr(ctx)
		if err != nil {
			// Redis communication failed, switch to local mode
			h.redisHealthy.Store(false)
			return h.local.Allow()
		}
		return allowed
	}
	// Redis unhealthy, use local rate limiter
	return h.local.Allow()
}

// redisHealthLoop periodically checks Redis health
func (h *HybridRateLimiter) redisHealthLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.checkRedisHealth()
		case <-h.checkStop:
			return
		case <-h.internalCtx.Done():
			return
		}
	}
}

// checkRedisHealth pings Redis and updates health status
func (h *HybridRateLimiter) checkRedisHealth() {
	ctx, cancel := context.WithTimeout(h.internalCtx, 2*time.Second)
	defer cancel()

	err := h.redisClient.Ping(ctx).Err()
	if err == nil {
		if !h.redisHealthy.Load() {
			// Redis recovered
			h.redisHealthy.Store(true)
		}
	} else {
		// Redis is down
		if h.redisHealthy.Load() {
			h.redisHealthy.Store(false)
		}
	}
}

// Shutdown stops the hybrid rate limiter
func (h *HybridRateLimiter) Shutdown() {
	h.stopOnce.Do(func() {
		close(h.checkStop)
		if h.internalCancel != nil {
			h.internalCancel()
		}
		h.wg.Wait()
	})
}
