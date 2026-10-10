package middleware

import "net/http"

// SecurityHeaders middleware adds standard security headers to HTTP responses.
//
// These headers help protect against common web vulnerabilities:
//   - X-Content-Type-Options: prevents MIME type sniffing (nosniff)
//   - X-Frame-Options: prevents clickjacking attacks (DENY)
//   - X-XSS-Protection: enables browser's XSS filter
//   - Referrer-Policy: controls referrer information leakage
//
// This middleware should be placed early in the middleware chain to ensure
// security headers are present on all responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		next.ServeHTTP(w, r)
	})
}
