package middleware

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/pkg/logger"
)

// rateLimitRejected counts HTTP requests rejected due to per-user rate limiting.
// Enables alerting on rate limit pressure and identification of abusive clients.
var rateLimitRejected = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "rtc_ratelimit_rejected_total",
		Help: "Total HTTP requests rejected by per-user rate limiter (429 responses).",
	},
)

// RateLimiter implements per-user token bucket rate limiting.
//
// Each authenticated user gets an independent rate.Limiter instance.
// Unauthenticated requests (no user ID in context) are passed through
// without rate limiting to avoid blocking health checks, OAuth endpoints, etc.
//
// The limiter uses a sync.Map for lock-free concurrent access to per-user
// limiters. Limiters are lazily created on first request from each user
// and cleaned up periodically to prevent unbounded memory growth.
type RateLimiter struct {
	limiters    sync.Map // map[string]*rate.Limiter
	lastSeen    sync.Map // map[string]time.Time (last access per user)
	r           rate.Limit
	burst       int
	stopCleanup chan struct{}
}

// NewRateLimiter creates a RateLimiter with the given rate and burst.
//
// r: tokens per second (e.g., rate.Limit(10) for 10 req/s)
// burst: maximum burst size (must be >= 1)
//
// Starts a background goroutine that periodically removes limiters
// not seen in the last 10 minutes.
func NewRateLimiter(r rate.Limit, burst int) *RateLimiter {
	rl := &RateLimiter{
		r:           r,
		burst:       burst,
		stopCleanup: make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// getLimiter returns the rate limiter for the given user ID,
// creating one if it doesn't exist.
func (rl *RateLimiter) getLimiter(userID string) *rate.Limiter {
	now := time.Now()
	rl.lastSeen.Store(userID, now)
	if v, ok := rl.limiters.Load(userID); ok {
		return v.(*rate.Limiter)
	}
	limiter := rate.NewLimiter(rl.r, rl.burst)
	actual, _ := rl.limiters.LoadOrStore(userID, limiter)
	return actual.(*rate.Limiter)
}

// cleanupLoop periodically removes limiters not seen in the last 10 minutes.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-rl.stopCleanup:
			return
		}
	}
}

// cleanup removes limiters that have not been accessed in the last 10 minutes.
func (rl *RateLimiter) cleanup() {
	cutoff := time.Now().Add(-10 * time.Minute)

	// Collect stale keys from lastSeen
	var staleKeys []string
	rl.lastSeen.Range(func(key, value any) bool {
		if lastTime, ok := value.(time.Time); ok && lastTime.Before(cutoff) {
			staleKeys = append(staleKeys, key.(string))
		}
		return true
	})

	// Remove stale limiters and lastSeen entries
	for _, key := range staleKeys {
		rl.limiters.Delete(key)
		rl.lastSeen.Delete(key)
	}

	if len(staleKeys) > 0 {
		logger.Debug(context.Background(), "ratelimiter.cleanup_removed_stale_limiters",
			zap.Int("removed", len(staleKeys)))
	}
}

// Stop stops the cleanup goroutine.
func (rl *RateLimiter) Stop() {
	select {
	case <-rl.stopCleanup:
	default:
		close(rl.stopCleanup)
	}
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
				rateLimitRejected.Inc()
				logger.Warn(r.Context(), "rate limit exceeded",
					zap.String("user_id", userID.String()),
				)
				httputil.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
