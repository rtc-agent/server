package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/rtc-agent/server/internal/infra/config"
)

// GinCSP returns a Gin middleware that sets Content-Security-Policy headers.
// When cfg.Enabled is false, the middleware passes through without setting headers.
// When cfg.ReportOnly is true, uses Content-Security-Policy-Report-Only header (non-blocking).
func GinCSP(cfg *config.CSPConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !cfg.Enabled {
			c.Next()
			return
		}

		csp := buildCSPHeader(cfg)
		headerName := "Content-Security-Policy"
		if cfg.ReportOnly {
			headerName = "Content-Security-Policy-Report-Only"
		}

		c.Header(headerName, csp)
		c.Next()
	}
}
