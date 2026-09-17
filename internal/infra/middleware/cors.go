package middleware

import (
	"net/http"
	"strings"
)

// CORS middleware handles Cross-Origin Resource Sharing.
// When allowedOrigins is empty and isDevelopment is true, falls back to "*" (any origin).
// In production (isDevelopment=false), allow_origins must be explicitly configured;
// otherwise no CORS headers are set (cross-origin requests are rejected).
func CORS(allowedOrigins []string, isDevelopment bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			originAllowed := false

			if len(allowedOrigins) == 0 {
				if isDevelopment {
					// Development-only fallback: allow any origin.
					originAllowed = true
					w.Header().Set("Access-Control-Allow-Origin", "*")
				}
				// Production + empty allow_origins: no CORS headers (reject cross-origin).
			} else if isOriginAllowed(origin, allowedOrigins) {
				originAllowed = true
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}

			// Only set additional CORS headers when the origin is allowed,
			// to avoid leaking capability information to unauthorized origins.
			if originAllowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func isOriginAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, "*") || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}
