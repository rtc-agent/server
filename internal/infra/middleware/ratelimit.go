package middleware

import (
	"net/http"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
)

// RateLimiter implements per-user token bucket rate limiting.
//
// Each authenticated user gets an independent rate.Limiter instance.
// Unauthenticated requests (no user ID in context) are passed through
// without rate limiting to avoid blocking health checks, OAuth endpoints, etc.
//
// The limiter uses a sync.Map for lock-free concurrent access to per-user
// limiters. Limiters are lazily created on first request from each user
// and retained for the lifetime of the process.
type RateLimiter struct {
	limiters sync.Map // map[string]*rate.Limiter
	r        rate.Limit
	burst    int
}

// NewRateLimiter creates a RateLimiter with the given rate and burst.
//
// r: tokens per second (e.g., rate.Limit(10) for 10 req/s)
// burst: maximum burst size (must be >= 1)
func NewRateLimiter(r rate.Limit, burst int) *RateLimiter {
	return &RateLimiter{r: r, burst: burst}
}

// getLimiter returns the rate limiter for the given user ID,
// creating one if it doesn't exist.
func (rl *RateLimiter) getLimiter(userID string) *rate.Limiter {
	if v, ok := rl.limiters.Load(userID); ok {
		return v.(*rate.Limiter)
	}
	limiter := rate.NewLimiter(rl.r, rl.burst)
	actual, _ := rl.limiters.LoadOrStore(userID, limiter)
	return actual.(*rate.Limiter)
}

// Middleware returns an HTTP middleware that enforces per-user rate limiting.
//
// Requests from authenticated users are rate-limited according to the
// configured rate and burst. When the limit is exceeded, a 429 Too Many
// Requests response is returned.
//
// Unauthenticated requests (no user ID in context) are passed through
// without rate limiting.
func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := contextx.GetUserID(r.Context())
			if !ok || userID == uuid.Nil {
				// No authenticated user - pass through without rate limiting
				next.ServeHTTP(w, r)
				return
			}
			limiter := rl.getLimiter(userID.String())
			if !limiter.Allow() {
				httputil.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
