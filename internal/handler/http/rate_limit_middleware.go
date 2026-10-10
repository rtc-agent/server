// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// RateLimiterInterface defines the contract for rate limiters.
// Both in-memory and Redis-backed implementations satisfy this interface.
type RateLimiterInterface interface {
	Allow(ip string) bool
	Stop()
}

// bucketShard is a sharded bucket map with its own lock.
// Reduces lock contention by partitioning IP addresses across multiple shards.
//
// TODO(observability): Add lock hold time metrics to identify contention hotspots:
//   - rate_limiter_lock_hold_duration_seconds: Histogram per shard
//   - rate_limiter_lock_acquisitions_total{status="acquired|contended"}: Counter
//
// These metrics help tune shard count and detect abnormal contention patterns.
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

	// Start periodic cleanup with panic-safe goroutine
	logger.SafeGo("rate-limiter-cleanup", func() {
		rl.cleanupLoop()
	})
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
	defer func() {
		if r := recover(); r != nil {
			logger.Error(context.Background(), "panic_recovered",
				zap.Any("panic", r),
				zap.String("goroutine", "rate_limiter.cleanupLoop"),
				zap.String("stack", string(debug.Stack())))
		}
	}()

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

// STSRateLimitMiddleware creates a standard HTTP middleware for rate limiting STS endpoints.
// Rate limiting is based on IP address to prevent credential exhaustion attacks.
//
// P1-7 fix: Current implementation uses IP-based rate limiting, which provides basic DoS protection.
// For more granular control, consider adding user-based rate limiting AFTER authentication:
//
// Recommended enhancement (two-tier rate limiting):
// 1. IP-based (current): Prevents DoS attacks from any single source IP.
//   - Runs before authentication (fast fail)
//   - Protects against credential stuffing from botnets
//   - Limit: 100 requests/minute/IP (configurable)
//
// 2. User-based (future enhancement): Prevents abuse by authenticated users.
//   - Runs after JWT authentication
//   - Uses userID as the rate limit key
//   - Limit: 10 requests/minute/user (configurable, stricter than IP-based)
//   - Implementation: Create a new middleware that extracts userID from context
//     and calls a separate user-based rate limiter (Redis-backed for distributed systems).
//
// Why IP-based is acceptable for now:
// - STS credentials are short-lived and scoped to the user's own files
// - Abuse is limited by the user's quota (can't upload beyond quota)
// - IP-based limiting catches most automated attacks
// - User-based limiting adds complexity (Redis dependency, distributed state)
//
// If abuse becomes an issue, implement user-based limiting as a second middleware layer.
func STSRateLimitMiddleware(limiter RateLimiterInterface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		// Extract IP from X-Forwarded-For or X-Real-IP if present (for proxied requests)
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			ip = forwarded
		} else if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
			ip = realIP
		}

		if !limiter.Allow(ip) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			resp := map[string]string{
				"error":             "rate_limit_exceeded",
				"error_description": "Too many requests. Please try again later.",
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		next.ServeHTTP(w, r)
	})
}
