package middleware

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/rtc-agent/server/internal/infra/httputil"
	"github.com/rtc-agent/server/pkg/logger"
)

// ipRateLimitRejected counts HTTP requests rejected due to per-IP rate limiting.
var ipRateLimitRejected = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "rtc_ip_ratelimit_rejected_total",
		Help: "Total HTTP requests rejected by per-IP rate limiter (429 responses).",
	},
)

// IPRateLimiter implements per-IP token bucket rate limiting.
//
// Each unique IP address gets an independent rate.Limiter instance.
// This is used to protect public endpoints (OAuth2, health checks) from abuse.
//
// The limiter uses a sync.Map for lock-free concurrent access. Limiters are
// lazily created on first request and have a TTL for automatic cleanup.
type IPRateLimiter struct {
	limiters sync.Map // map[string]*ipLimiterEntry
	r        rate.Limit
	burst    int
	ttl      time.Duration
}

type ipLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // Unix nano, accessed atomically to avoid data races
}

// NewIPRateLimiter creates an IPRateLimiter with the given rate, burst, and TTL.
//
// r: tokens per second (e.g., rate.Limit(5) for 5 req/s per IP)
// burst: maximum burst size (must be >= 1)
// ttl: duration after which an unused limiter is eligible for cleanup
func NewIPRateLimiter(r rate.Limit, burst int, ttl time.Duration) *IPRateLimiter {
	return &IPRateLimiter{r: r, burst: burst, ttl: ttl}
}

// getLimiter returns the rate limiter for the given IP,
// creating one if it doesn't exist.
func (rl *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
	now := time.Now()
	if v, ok := rl.limiters.Load(ip); ok {
		entry := v.(*ipLimiterEntry)
		entry.lastSeen.Store(now.UnixNano())
		return entry.limiter
	}
	limiter := rate.NewLimiter(rl.r, rl.burst)
	entry := &ipLimiterEntry{limiter: limiter}
	entry.lastSeen.Store(now.UnixNano())
	actual, _ := rl.limiters.LoadOrStore(ip, entry)
	return actual.(*ipLimiterEntry).limiter
}

// Cleanup removes entries that haven't been seen within the TTL.
// Should be called periodically (e.g., every minute) to prevent memory growth.
func (rl *IPRateLimiter) Cleanup() {
	cutoff := time.Now().Add(-rl.ttl).UnixNano()
	rl.limiters.Range(func(key, value any) bool {
		entry := value.(*ipLimiterEntry)
		if entry.lastSeen.Load() < cutoff {
			rl.limiters.Delete(key)
		}
		return true
	})
}

// extractClientIP extracts the client IP from the request.
// It checks X-Forwarded-For and X-Real-IP headers first (for proxied requests),
// then falls back to RemoteAddr.
func extractClientIP(r *http.Request) string {
	// Check X-Forwarded-For first (may contain multiple IPs, take the first)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For: client, proxy1, proxy2
		if idx := net.ParseIP(xff); idx != nil {
			return idx.String()
		}
		// Multiple IPs - take the first one
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				if ip := net.ParseIP(xff[:i]); ip != nil {
					return ip.String()
				}
				break
			}
		}
	}

	// Check X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return ip.String()
		}
	}

	// Fall back to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // No port
	}
	return host
}

// Middleware returns an HTTP middleware that enforces per-IP rate limiting.
//
// Requests from each unique IP are rate-limited according to the configured
// rate and burst. When the limit is exceeded, a 429 Too Many Requests
// response is returned.
func (rl *IPRateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := extractClientIP(r)
			limiter := rl.getLimiter(ip)
			if !limiter.Allow() {
				ipRateLimitRejected.Inc()
				logger.Warn(r.Context(), "IP rate limit exceeded",
					zap.String("remote_addr", r.RemoteAddr),
					zap.String("client_ip", ip))
				httputil.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests from your IP")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WrapHandler wraps a single handler with IP rate limiting.
func (rl *IPRateLimiter) WrapHandler(h http.Handler) http.Handler {
	return rl.Middleware()(h)
}

// WrapHandlerFunc wraps a handler function with IP rate limiting.
func (rl *IPRateLimiter) WrapHandlerFunc(h http.HandlerFunc) http.HandlerFunc {
	return rl.Middleware()(h).(http.HandlerFunc)
}
