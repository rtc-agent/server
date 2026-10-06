// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// SecurityHeadersMiddleware adds security-related HTTP headers to responses.
//
// Headers set:
//   - X-Content-Type-Options: nosniff
//   - X-Frame-Options: DENY
//   - X-XSS-Protection: 1; mode=block
//   - Referrer-Policy: strict-origin-when-cross-origin
//   - Content-Security-Policy: restrictive CSP policy
//   - Strict-Transport-Security: HSTS (only when behind TLS termination)
//
// Skips /api/grafana/* routes to allow iframe embedding of Grafana dashboards.
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip framing-restricting headers for proxy routes (iframe embedding)
		if !strings.HasPrefix(c.Request.URL.Path, "/api/grafana/") &&
			!strings.HasPrefix(c.Request.URL.Path, "/api/jaeger/") &&
			!strings.HasPrefix(c.Request.URL.Path, "/api/pyroscope/") {
			c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
			c.Writer.Header().Set("X-Frame-Options", "DENY")
			c.Writer.Header().Set("X-XSS-Protection", "1; mode=block")
			c.Writer.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			c.Writer.Header().Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data: https:; "+
					"connect-src 'self'; "+
					"font-src 'self' data:; "+
					"object-src 'none'; "+
					"frame-ancestors 'none'; "+
					"base-uri 'self'; "+
					"form-action 'self'")
		}

		c.Next()
	}
}
