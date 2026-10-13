package svc

import (
	stdcontext "context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/rtc-agent/server/pkg/logger"
)

// WebSocket authentication rate limiting metrics.
var (
	// wsAuthRateLimitRejected counts WebSocket connections rejected due to rate limiting.
	wsAuthRateLimitRejected = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "rtc_ws_auth_ratelimit_rejected_total",
			Help: "Total WebSocket connection attempts rejected due to authentication rate limiting.",
		},
	)

	// wsAuthFailureTotal counts authentication failures.
	wsAuthFailureTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_ws_auth_failure_total",
			Help: "Total WebSocket authentication failures by reason.",
		},
		[]string{"reason"},
	)
)

// AuthFailureLimiter implements per-IP rate limiting for WebSocket authentication failures.
//
// This prevents brute-force attacks by limiting the number of failed authentication
// attempts from a single IP address. When an IP exceeds the rate limit, subsequent
// connection attempts are temporarily rejected with a clear error message.
//
// The limiter uses an in-memory token bucket algorithm with automatic cleanup of
// stale entries to prevent memory growth.
type AuthFailureLimiter struct {
	limiters       sync.Map // map[string]*authLimiterEntry
	r              rate.Limit
	burst          int
	ttl            time.Duration
	blockDuration  time.Duration
	trustedProxies []*net.IPNet
}

type authLimiterEntry struct {
	failureLimiter *rate.Limiter
	blockUntil     atomic.Int64 // Unix nano timestamp, 0 means not blocked
	lastSeen       atomic.Int64 // Unix nano timestamp
}

// newAuthLimiterEntry creates a new entry with a fresh rate limiter.
func newAuthLimiterEntry(r rate.Limit, burst int) *authLimiterEntry {
	entry := &authLimiterEntry{
		failureLimiter: rate.NewLimiter(r, burst),
	}
	now := time.Now()
	entry.lastSeen.Store(now.UnixNano())
	return entry
}

// resetFailureLimiter creates a new rate limiter, replacing the old one.
func (e *authLimiterEntry) resetFailureLimiter(r rate.Limit, burst int) {
	e.failureLimiter = rate.NewLimiter(r, burst)
}

// NewAuthFailureLimiter creates a rate limiter for WebSocket authentication failures.
//
// Parameters:
//   - r: tokens per second for failure attempts (e.g., rate.Limit(0.166) for 10 per minute)
//   - burst: maximum burst size (e.g., 10 for allowing 10 quick retries)
//   - ttl: duration after which an unused entry is eligible for cleanup
//   - blockDuration: how long to block an IP after exceeding the rate limit
func NewAuthFailureLimiter(r rate.Limit, burst int, ttl time.Duration, blockDuration time.Duration) *AuthFailureLimiter {
	return &AuthFailureLimiter{
		r:             r,
		burst:         burst,
		ttl:           ttl,
		blockDuration: blockDuration,
	}
}

// SetTrustedProxies configures CIDR ranges of trusted reverse proxies.
// When set, X-Forwarded-For and X-Real-IP headers are only trusted if the
// request's RemoteAddr is within one of these ranges.
func (l *AuthFailureLimiter) SetTrustedProxies(cidrs []string) error {
	var nets []*net.IPNet
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return err
		}
		nets = append(nets, ipNet)
	}
	l.trustedProxies = nets
	return nil
}

// extractClientIP extracts the client IP from the context.
// The context should contain the HTTP request with RemoteAddr or X-Forwarded-For.
func (l *AuthFailureLimiter) extractClientIP(ctx stdcontext.Context) string {
	// Try to get the HTTP request from context using the standard key
	req, ok := ctx.Value(http.ServerContextKey).(*http.Request)
	if !ok || req == nil {
		// Fallback: try to extract from context values that might contain IP info
		// This is a fallback for cases where the context doesn't have the full request
		return "unknown"
	}

	remoteIP := parseRemoteIPFromRequest(req)

	if isTrustedProxyIP(remoteIP, l.trustedProxies) {
		// Check X-Forwarded-For first (may contain multiple IPs, take the first)
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			for i := 0; i < len(xff); i++ {
				if xff[i] == ',' {
					if ip := net.ParseIP(xff[:i]); ip != nil {
						return ip.String()
					}
					break
				}
			}
			if ip := net.ParseIP(xff); ip != nil {
				return ip.String()
			}
		}

		// Check X-Real-IP (single IP)
		if xri := req.Header.Get("X-Real-IP"); xri != "" {
			if ip := net.ParseIP(xri); ip != nil {
				return ip.String()
			}
		}
	}

	return remoteIP.String()
}

// parseRemoteIPFromRequest extracts the IP from the request's RemoteAddr.
func parseRemoteIPFromRequest(req *http.Request) net.IP {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return net.ParseIP(req.RemoteAddr)
	}
	return net.ParseIP(host)
}

// isTrustedProxyIP checks if the given IP is within any of the trusted proxy ranges.
func isTrustedProxyIP(ip net.IP, trustedProxies []*net.IPNet) bool {
	if ip == nil || len(trustedProxies) == 0 {
		return false
	}
	for _, n := range trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// RecordFailure records an authentication failure for the given IP.
// Returns true if the IP should be blocked, false otherwise.
func (l *AuthFailureLimiter) RecordFailure(ctx stdcontext.Context, ip string) bool {
	now := time.Now()
	entry := l.getEntry(ip)
	entry.lastSeen.Store(now.UnixNano())

	// Check if currently blocked
	if blockUntil := entry.blockUntil.Load(); blockUntil > 0 {
		if now.UnixNano() < blockUntil {
			return true // Still blocked
		}
		// Block period expired, reset
		entry.blockUntil.Store(0)
		entry.resetFailureLimiter(l.r, l.burst)
	}

	// Check if rate limit exceeded
	if !entry.failureLimiter.Allow() {
		// Block the IP
		blockUntil := now.Add(l.blockDuration).UnixNano()
		entry.blockUntil.Store(blockUntil)

		logger.Warn(ctx, "[WebSocket Auth] IP rate limit exceeded, blocking temporarily",
			zap.String("client_ip", ip),
			zap.Time("block_until", time.Unix(0, blockUntil)),
			zap.Duration("block_duration", l.blockDuration),
		)

		return true
	}

	return false
}

// IsBlocked checks if the given IP is currently blocked.
func (l *AuthFailureLimiter) IsBlocked(ctx stdcontext.Context, ip string) bool {
	entry, ok := l.limiters.Load(ip)
	if !ok {
		return false
	}

	e := entry.(*authLimiterEntry)
	blockUntil := e.blockUntil.Load()
	if blockUntil == 0 {
		return false
	}

	if time.Now().UnixNano() < blockUntil {
		return true
	}

	// Block period expired, reset
	e.blockUntil.Store(0)
	e.resetFailureLimiter(l.r, l.burst)
	return false
}

// getEntry returns the rate limiter entry for the given IP,
// creating one if it doesn't exist.
func (l *AuthFailureLimiter) getEntry(ip string) *authLimiterEntry {
	now := time.Now()
	if v, ok := l.limiters.Load(ip); ok {
		entry := v.(*authLimiterEntry)
		entry.lastSeen.Store(now.UnixNano())
		return entry
	}

	entry := newAuthLimiterEntry(l.r, l.burst)
	actual, _ := l.limiters.LoadOrStore(ip, entry)
	return actual.(*authLimiterEntry)
}

// Cleanup removes entries that haven't been seen within the TTL.
// Should be called periodically to prevent memory growth.
func (l *AuthFailureLimiter) Cleanup() {
	cutoff := time.Now().Add(-l.ttl).UnixNano()
	l.limiters.Range(func(key, value any) bool {
		entry := value.(*authLimiterEntry)
		if entry.lastSeen.Load() < cutoff {
			l.limiters.Delete(key)
		}
		return true
	})
}

// RecordSuccess records a successful authentication.
// This is optional but can be used for metrics.
func (l *AuthFailureLimiter) RecordSuccess(ctx stdcontext.Context, ip string) {
	// Currently a no-op, but can be extended to track success metrics
	// or to reset failure counters on successful authentication.
}

// DisconnectReasonTooManyAttempts is returned when an IP exceeds the rate limit.
var DisconnectReasonTooManyAttempts = centrifuge.Disconnect{
	Code:   4503,
	Reason: "too many authentication attempts",
}

// CleanupLoop runs a background goroutine that periodically cleans up stale entries.
// It should be started once when the server starts.
func (l *AuthFailureLimiter) CleanupLoop(ctx stdcontext.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.Cleanup()
		case <-ctx.Done():
			return
		}
	}
}
