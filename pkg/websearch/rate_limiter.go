package websearch

import (
	"golang.org/x/time/rate"
)

// RateLimiterConfig defines rate limiter parameters
type RateLimiterConfig struct {
	Rate  float64 `json:"rate"`  // Requests per second
	Burst int     `json:"burst"` // Burst capacity
}

// DefaultRateLimiterConfig returns sensible defaults
func DefaultRateLimiterConfig() RateLimiterConfig {
	return RateLimiterConfig{
		Rate:  10,
		Burst: 20,
	}
}

// RateLimiter wraps golang.org/x/time/rate for production use
type RateLimiter struct {
	limiter *rate.Limiter
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	return &RateLimiter{
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
	}
}

// Allow checks if a request is allowed
func (r *RateLimiter) Allow() bool {
	return r.limiter.Allow()
}
