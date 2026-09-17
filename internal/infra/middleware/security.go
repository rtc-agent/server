package middleware

import (
	"net/http"
	"strings"
)

// SecurityHeaders middleware adds common security headers to all responses,
// protecting against common web attacks.
// WebSocket upgrade requests are skipped to avoid interfering with protocol upgrade.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip WebSocket upgrade requests to avoid interfering with protocol upgrade.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'")

		next.ServeHTTP(w, r)
	})
}
