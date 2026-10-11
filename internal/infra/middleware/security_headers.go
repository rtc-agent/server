package middleware

import "net/http"

// SecurityHeaders middleware adds standard security headers to HTTP responses.
//
// These headers help protect against common web vulnerabilities:
//   - X-Content-Type-Options: prevents MIME type sniffing (nosniff)
//   - X-Frame-Options: prevents clickjacking attacks (DENY)
//   - X-XSS-Protection: enables browser's XSS filter
//   - Referrer-Policy: controls referrer information leakage
//   - Strict-Transport-Security: enforces HTTPS connections (when TLS is active)
//
// This middleware should be placed early in the middleware chain to ensure
// security headers are present on all responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// HSTS: only enable when the request arrived over TLS (direct or via reverse proxy).
		// Prevents downgrade attacks by instructing browsers to always use HTTPS.
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}
