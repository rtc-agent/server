package webfetch

import (
	"sync"

	"golang.org/x/time/rate"
)

// RateLimitConfig holds rate limiter parameters.
type RateLimitConfig struct {
	GlobalRPS   float64 // global requests per second, default 20
	GlobalBurst int     // global burst, default 40
	DomainRPS   float64 // per-domain requests per second, default 2
	DomainBurst int     // per-domain burst, default 5
}

// DefaultRateLimitConfig returns sensible defaults.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		GlobalRPS:   20,
		GlobalBurst: 40,
		DomainRPS:   2,
		DomainBurst: 5,
	}
}

// WebFetchRateLimiter implements dual-layer rate limiting:
// global QPS + per-domain QPS.
type WebFetchRateLimiter struct {
	global         *rate.Limiter
	domainLimiters map[string]*rate.Limiter
	mu             sync.Mutex
	config         RateLimitConfig
}

// NewWebFetchRateLimiter creates a dual-layer rate limiter.
func NewWebFetchRateLimiter(config RateLimitConfig) *WebFetchRateLimiter {
	return &WebFetchRateLimiter{
		global:         rate.NewLimiter(rate.Limit(config.GlobalRPS), config.GlobalBurst),
		domainLimiters: make(map[string]*rate.Limiter),
		config:         config,
	}
}

// Allow checks both global and per-domain rate limits.
// Returns true if the request is allowed, false if rate-limited.
func (r *WebFetchRateLimiter) Allow(domain string) bool {
	// Check global limit first (fail-fast).
	if !r.global.Allow() {
		return false
	}

	// Check per-domain limit.
	r.mu.Lock()
	limiter, ok := r.domainLimiters[domain]
	if !ok {
		limiter = rate.NewLimiter(rate.Limit(r.config.DomainRPS), r.config.DomainBurst)
		r.domainLimiters[domain] = limiter
	}
	r.mu.Unlock()

	return limiter.Allow()
}

// cleanup removes idle domain limiters. Called periodically.
func (r *WebFetchRateLimiter) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// For now, we keep all domain limiters since rate.Limiter doesn't
	// expose its last-use time. In the future, consider using an LRU
	// or tracking last-access timestamps.
}
