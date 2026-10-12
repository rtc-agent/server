package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rtc-agent/server/internal/infra/config"
)

// CSP returns a standard library HTTP middleware that sets Content-Security-Policy headers.
// When cfg.Enabled is false, the middleware passes through without setting headers.
// When cfg.ReportOnly is true, uses Content-Security-Policy-Report-Only header (non-blocking).
func CSP(cfg *config.CSPConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			csp := buildCSPHeader(cfg)
			headerName := "Content-Security-Policy"
			if cfg.ReportOnly {
				headerName = "Content-Security-Policy-Report-Only"
			}

			w.Header().Set(headerName, csp)
			next.ServeHTTP(w, r)
		})
	}
}

// buildCSPHeader constructs the CSP header value from configuration.
// Each directive is formatted as "directive-name value1 value2 ..."
// and directives are joined with "; ".
// This is a shared implementation used by both standard library and Gin middlewares.
func buildCSPHeader(cfg *config.CSPConfig) string {
	parts := make([]string, 0, 12) // 11 directives + report-uri

	// Helper to add a directive if values are present
	addDirective := func(name string, values []string) {
		if len(values) > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", name, strings.Join(values, " ")))
		}
	}

	// Add each CSP directive
	addDirective("default-src", cfg.DefaultSrc)
	addDirective("script-src", cfg.ScriptSrc)
	addDirective("style-src", cfg.StyleSrc)
	addDirective("img-src", cfg.ImgSrc)
	addDirective("font-src", cfg.FontSrc)
	addDirective("media-src", cfg.MediaSrc)
	addDirective("connect-src", cfg.ConnectSrc)
	addDirective("frame-src", cfg.FrameSrc)
	addDirective("frame-ancestors", cfg.FrameAncestors)
	addDirective("base-uri", cfg.BaseURI)
	addDirective("form-action", cfg.FormAction)

	// Add report-uri if configured
	if cfg.ReportURI != "" {
		parts = append(parts, fmt.Sprintf("report-uri %s", cfg.ReportURI))
	}

	return strings.Join(parts, "; ")
}
