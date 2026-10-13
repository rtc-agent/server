package middleware

import (
	"net"
	"net/http"
	"strings"
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
	limiters       sync.Map // map[string]*ipLimiterEntry
	r              rate.Limit
	burst          int
	ttl            time.Duration
	trustedProxies []*net.IPNet // CIDR ranges of trusted proxies; if empty, headers are not trusted
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

// SetTrustedProxies configures CIDR ranges of trusted reverse proxies.
// When set, X-Forwarded-For and X-Real-IP headers are only trusted if the
// request's RemoteAddr is within one of these ranges. This prevents IP spoofing
// when the service is exposed directly to the internet without a proxy.
func (rl *IPRateLimiter) SetTrustedProxies(cidrs []string) error {
	var nets []*net.IPNet
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return err
		}
		nets = append(nets, ipNet)
	}
	rl.trustedProxies = nets
	return nil
}

// GetLimiter returns the rate limiter for the given IP,
// creating one if it doesn't exist.
//
// This is exported so that handlers can apply per-IP rate limiting
// at specific points within their logic (e.g., Token Exchange endpoint).
// For wrapping entire handlers, prefer Middleware() or WrapHandlerFunc().
func (rl *IPRateLimiter) GetLimiter(ip string) *rate.Limiter {
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

// ExtractClientIP extracts the client IP from the request, only trusting
// X-Forwarded-For and X-Real-IP headers if the request comes from a trusted proxy.
// This is a package-level function that can be reused by other middleware.
//
// SECURITY NOTES:
// - X-Forwarded-For can contain multiple IPs: "client, proxy1, proxy2"
// - We take the FIRST IP as the client IP (leftmost)
// - Subsequent IPs are proxy chain, which we don't trust for rate limiting
// - If X-Forwarded-For is malformed, we fall back to RemoteAddr
func ExtractClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remoteIP := parseRemoteIP(r)

	if isTrustedProxyIP(remoteIP, trustedProxies) {
		// Check X-Forwarded-For first (may contain multiple IPs, take the first)
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			for i := 0; i < len(xff); i++ {
				if xff[i] == ',' {
					if ip := net.ParseIP(strings.TrimSpace(xff[:i])); ip != nil {
						return ip.String()
					}
					break
				}
			}
			if ip := net.ParseIP(strings.TrimSpace(xff)); ip != nil {
				return ip.String()
			}
		}

		// Check X-Real-IP (single IP)
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			if ip := net.ParseIP(strings.TrimSpace(xri)); ip != nil {
				return ip.String()
			}
		}
	}

	return remoteIP.String()
}

// extractClientIP extracts the client IP from the request.
// It only trusts X-Forwarded-For and X-Real-IP headers if the request comes
// from a trusted proxy (configured via SetTrustedProxies). Otherwise, it falls
// back to RemoteAddr to prevent IP spoofing.
//
// SECURITY NOTES:
// - X-Forwarded-For can contain multiple IPs: "client, proxy1, proxy2"
// - We take the FIRST IP as the client IP (leftmost)
// - Subsequent IPs are proxy chain, which we don't trust for rate limiting
// - If X-Forwarded-For is malformed, we fall back to RemoteAddr
func (rl *IPRateLimiter) extractClientIP(r *http.Request) string {
	return ExtractClientIP(r, rl.trustedProxies)
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

// parseRemoteIP extracts the IP from the request's RemoteAddr.
func parseRemoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return net.ParseIP(r.RemoteAddr)
	}
	return net.ParseIP(host)
}

// Middleware returns an HTTP middleware that enforces per-IP rate limiting.
//
// Requests from each unique IP are rate-limited according to the configured
// rate and burst. When the limit is exceeded, a 429 Too Many Requests
// response is returned.
func (rl *IPRateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := rl.extractClientIP(r)
			limiter := rl.GetLimiter(ip)
			if !limiter.Allow() {
				ipRateLimitRejected.Inc()
				logger.Warn(r.Context(), "IP rate limit exceeded",
					zap.String("remote_addr", r.RemoteAddr),
					zap.String("client_ip", ip))
				httputil.WriteError(r.Context(), w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests from your IP")
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
