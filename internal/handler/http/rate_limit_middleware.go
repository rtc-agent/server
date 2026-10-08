// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiterInterface defines the contract for rate limiters.
// Both in-memory and Redis-backed implementations satisfy this interface.
type RateLimiterInterface interface {
	Allow(ip string) bool
	Stop()
}

// bucketShard is a sharded bucket map with its own lock.
// Reduces lock contention by partitioning IP addresses across multiple shards.
type bucketShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// RateLimiter implements a sharded token bucket rate limiter per IP address.
//
// Uses multiple shards (default 64) to reduce lock contention under high concurrency.
// Each shard has its own mutex, so requests to different shards can proceed in parallel.
// For distributed rate limiting across multiple instances, use RedisRateLimiter.
type RateLimiter struct {
	shards    []*bucketShard
	numShards uint32
	rate      int           // max requests per window
	window    time.Duration // window duration
	cleanup   time.Duration // cleanup interval
	stopCh    chan struct{}
}

type bucket struct {
	tokens   int
	lastFill time.Time
}

// RateLimitConfig configures the rate limiter.
type RateLimitConfig struct {
	// MaxRequestsPerMinute is the maximum number of requests allowed per minute per IP.
	MaxRequestsPerMinute int
	// CleanupInterval is how often to clean up expired buckets.
	CleanupInterval time.Duration
	// NumShards is the number of lock shards for reducing contention (default 64).
	// Higher values reduce lock contention but use more memory.
	NumShards int
}

// NewRateLimiter creates a new RateLimiter with sharded locks.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.MaxRequestsPerMinute <= 0 {
		cfg.MaxRequestsPerMinute = 100
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 5 * time.Minute
	}
	if cfg.NumShards <= 0 {
		cfg.NumShards = 64
	}

	// Initialize shards
	shards := make([]*bucketShard, cfg.NumShards)
	for i := range shards {
		shards[i] = &bucketShard{
			buckets: make(map[string]*bucket),
		}
	}

	rl := &RateLimiter{
		shards:    shards,
		numShards: uint32(cfg.NumShards),
		rate:      cfg.MaxRequestsPerMinute,
		window:    time.Minute,
		cleanup:   cfg.CleanupInterval,
		stopCh:    make(chan struct{}),
	}

	// Start periodic cleanup
	go rl.cleanupLoop()
	return rl
}

// getShard returns the shard for a given IP address using FNV-1a hash.
func (rl *RateLimiter) getShard(ip string) *bucketShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ip))
	return rl.shards[h.Sum32()%rl.numShards]
}

// Allow checks whether the request from the given IP should be allowed.
// Uses sharded locks to minimize contention — only the shard for this IP is locked.
func (rl *RateLimiter) Allow(ip string) bool {
	shard := rl.getShard(ip)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()
	b, exists := shard.buckets[ip]

	if !exists {
		shard.buckets[ip] = &bucket{
			tokens:   rl.rate - 1,
			lastFill: now,
		}
		return true
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(b.lastFill)
	if elapsed >= rl.window {
		b.tokens = rl.rate
		b.lastFill = now
	}

	if b.tokens <= 0 {
		return false
	}

	b.tokens--
	return true
}

// cleanupLoop periodically removes expired buckets.
// Each tick cleans only one shard (round-robin) to minimize lock hold time.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanup)
	defer ticker.Stop()

	shardIdx := uint32(0)
	for {
		select {
		case <-ticker.C:
			shard := rl.shards[shardIdx]
			shard.mu.Lock()
			now := time.Now()
			for ip, b := range shard.buckets {
				if now.Sub(b.lastFill) > rl.window*2 {
					delete(shard.buckets, ip)
				}
			}
			shard.mu.Unlock()
			shardIdx = (shardIdx + 1) % rl.numShards
		case <-rl.stopCh:
			return
		}
	}
}

// Stop stops the cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

// AdminRateLimitMiddleware creates a Gin middleware that enforces rate limiting for admin API.
// Grafana proxy routes (/api/grafana/*) are exempted — they are reverse-proxied to a local
// Grafana instance and generate high request volume from dashboard auto-refresh.
func AdminRateLimitMiddleware(limiter RateLimiterInterface) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip rate limiting for proxy routes (dashboard auto-refresh generates many requests)
		if strings.HasPrefix(c.Request.URL.Path, "/api/grafana/") ||
			strings.HasPrefix(c.Request.URL.Path, "/api/jaeger/") ||
			strings.HasPrefix(c.Request.URL.Path, "/api/pyroscope/") {
			c.Next()
			return
		}

		ip := c.ClientIP()
		if !limiter.Allow(ip) {
			Error(c, "rate_limit_exceeded",
				"请求过于频繁，请稍后重试")
			c.Abort()
			return
		}
		c.Next()
	}
}
