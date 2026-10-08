package middleware

import (
	"net/http"
	"strings"
)

// CORSAllowHeaders is the shared list of allowed headers for CORS preflight.
// Used by both the main API CORS middleware and the OSS3 S3 endpoint CORS middleware.
// Includes standard HTTP headers and AWS S3 SDK specific headers (x-amz-*).
const CORSAllowHeaders = "Content-Type, Authorization, Content-Length, Content-Encoding, " +
	"amz-sdk-invocation-id, amz-sdk-request, " +
	"x-amz-content-sha256, x-amz-date, x-amz-security-token, x-amz-user-agent, " +
	"x-amz-sdk-checksum-algorithm, x-amz-decoded-content-length, x-amz-trailer, " +
	"x-amz-checksum-crc32, x-amz-checksum-crc32c, x-amz-checksum-crc64nvme, " +
	"x-amz-checksum-sha1, x-amz-checksum-sha256, x-amz-checksum-sha512, x-amz-checksum-md5, " +
	"x-amz-acl, x-amz-copy-source, x-amz-meta-*, " +
	"x-amz-storage-class, x-amz-tagging, " +
	"x-amz-server-side-encryption, x-amz-website-redirect-location"

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
			} else if IsOriginAllowed(origin, allowedOrigins) {
				originAllowed = true
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}

			// Only set additional CORS headers when the origin is allowed,
			// to avoid leaking capability information to unauthorized origins.
			if originAllowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", CORSAllowHeaders)
				w.Header().Set("Access-Control-Expose-Headers", "ETag, x-amz-request-id, x-amz-id-2, x-amz-version-id")
				w.Header().Set("Access-Control-Max-Age", "3600")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// IsOriginAllowed checks if the origin is in the allowed list.
// Exported for reuse by other CORS implementations (e.g., OSS3).
func IsOriginAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, "*") || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}
