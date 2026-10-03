package webfetch

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig holds rate limiter parameters.
type RateLimitConfig struct {
	GlobalRPS   float64 // global requests per second, default 20
	GlobalBurst int     // global burst, default 40
	DomainRPS   float64 // per-domain requests per second, default 2
	DomainBurst int     // per-domain burst, default 5
	// MaxDomainLimiters is the maximum number of per-domain limiters to keep.
	// When exceeded, oldest entries are evicted. 0 means no limit (not recommended).
	// Default: 10000.
	MaxDomainLimiters int
}

// DefaultRateLimitConfig returns sensible defaults.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		GlobalRPS:         20,
		GlobalBurst:       40,
		DomainRPS:         2,
		DomainBurst:       5,
		MaxDomainLimiters: 10000,
	}
}

// domainLimiterEntry tracks a per-domain limiter with its last access time.
type domainLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

// WebFetchRateLimiter implements dual-layer rate limiting:
// global QPS + per-domain QPS.
type WebFetchRateLimiter struct {
	global         *rate.Limiter
	domainLimiters map[string]*domainLimiterEntry
	mu             sync.Mutex
	config         RateLimitConfig
}

// NewWebFetchRateLimiter creates a dual-layer rate limiter.
func NewWebFetchRateLimiter(config RateLimitConfig) *WebFetchRateLimiter {
	if config.MaxDomainLimiters <= 0 {
		config.MaxDomainLimiters = 10000
	}
	return &WebFetchRateLimiter{
		global:         rate.NewLimiter(rate.Limit(config.GlobalRPS), config.GlobalBurst),
		domainLimiters: make(map[string]*domainLimiterEntry),
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
	entry, ok := r.domainLimiters[domain]
	if !ok {
		// Evict oldest entries if we've reached the limit.
		if len(r.domainLimiters) >= r.config.MaxDomainLimiters {
			r.evictOldest()
		}
		entry = &domainLimiterEntry{
			limiter:    rate.NewLimiter(rate.Limit(r.config.DomainRPS), r.config.DomainBurst),
			lastAccess: time.Now(),
		}
		r.domainLimiters[domain] = entry
	} else {
		entry.lastAccess = time.Now()
	}
	r.mu.Unlock()

	return entry.limiter.Allow()
}

// evictOldest removes the oldest half of domain limiters.
// Must be called with r.mu held.
func (r *WebFetchRateLimiter) evictOldest() {
	// Find the median lastAccess time.
	times := make([]time.Time, 0, len(r.domainLimiters))
	for _, entry := range r.domainLimiters {
		times = append(times, entry.lastAccess)
	}
	if len(times) == 0 {
		return
	}
	// Simple approach: evict entries older than the median.
	// Sort times to find median.
	sortTimes(times)
	median := times[len(times)/2]

	for domain, entry := range r.domainLimiters {
		if entry.lastAccess.Before(median) {
			delete(r.domainLimiters, domain)
		}
	}
}

// sortTimes sorts a slice of times in ascending order using insertion sort.
// For small slices this is efficient; for larger slices we could use sort.Slice.
func sortTimes(times []time.Time) {
	for i := 1; i < len(times); i++ {
		for j := i; j > 0 && times[j].Before(times[j-1]); j-- {
			times[j], times[j-1] = times[j-1], times[j]
		}
	}
}
