// Package httphandler provides HTTP handler implementations.
package httphandler

import (
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

// RateLimiter implements an in-memory token bucket rate limiter per IP address.
//
// Uses an in-memory store with automatic cleanup of expired entries.
// For distributed rate limiting across multiple instances, use RedisRateLimiter.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    int           // max requests per window
	window  time.Duration // window duration
	cleanup time.Duration // cleanup interval
	stopCh  chan struct{}
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
}

// NewRateLimiter creates a new RateLimiter.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.MaxRequestsPerMinute <= 0 {
		cfg.MaxRequestsPerMinute = 100
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 5 * time.Minute
	}

	rl := &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    cfg.MaxRequestsPerMinute,
		window:  time.Minute,
		cleanup: cfg.CleanupInterval,
		stopCh:  make(chan struct{}),
	}

	// Start periodic cleanup
	go rl.cleanupLoop()
	return rl
}

// Allow checks whether the request from the given IP should be allowed.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[ip]

	if !exists {
		rl.buckets[ip] = &bucket{
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
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanup)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, b := range rl.buckets {
				if now.Sub(b.lastFill) > rl.window*2 {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
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
		// Skip rate limiting for Grafana proxy (dashboard auto-refresh generates many requests)
		if strings.HasPrefix(c.Request.URL.Path, "/api/grafana/") {
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
