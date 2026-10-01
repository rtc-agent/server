package httphandler

import (
	"context"
	"net/http"
	"strings"

	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// NewOSS3CORSMiddleware creates a CORS middleware for the OSS3 S3 endpoint.
//
// Unlike the main API CORS middleware, this one:
//   - Is designed for the independent S3 port (:9000)
//   - Allows S3-specific headers (x-amz-*, Authorization, Content-Type)
//   - Exposes S3-specific response headers (ETag, x-amz-request-id)
//   - Handles OPTIONS preflight for presigned URL uploads from browsers
//
// When origins is empty, all origins are denied (secure default).
// When origins contains "*", all origins are allowed (with warning log).
func NewOSS3CORSMiddleware(origins []string) func(http.Handler) http.Handler {
	// Log warning if wildcard is used
	if containsOSS3Wildcard(origins) {
		logger.Warn(context.Background(), "CORS allows all origins - this is insecure for production",
			zap.Strings("allowed_origins", origins))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Empty origins list means deny all CORS requests
			if len(origins) == 0 {
				w.WriteHeader(http.StatusForbidden)
				return
			}

			allowOrigin := ""
			if containsOSS3Wildcard(origins) {
				allowOrigin = "*"
			} else if isOSS3OriginAllowed(origin, origins) {
				allowOrigin = origin
			} else {
				w.WriteHeader(http.StatusForbidden)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, DELETE, HEAD, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-amz-*, x-amz-acl, x-amz-date, x-amz-content-sha256")
			w.Header().Set("Access-Control-Expose-Headers", "ETag, x-amz-request-id, x-amz-id-2, x-amz-version-id")
			w.Header().Set("Access-Control-Max-Age", "3600")
			if allowOrigin != "*" {
				w.Header().Set("Vary", "Origin")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// containsOSS3Wildcard returns true if any entry in the list is "*".
func containsOSS3Wildcard(origins []string) bool {
	for _, o := range origins {
		if o == "*" {
			return true
		}
	}
	return false
}

// isOSS3OriginAllowed checks if the origin is in the allowed list.
func isOSS3OriginAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, "*") || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}
