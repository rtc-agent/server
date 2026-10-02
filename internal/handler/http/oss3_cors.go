package httphandler

import (
	"context"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/middleware"
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

			// Non-browser requests (CLI/SDK) have no Origin header; skip CORS checks.
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Empty origins list means deny all CORS requests
			if len(origins) == 0 {
				w.WriteHeader(http.StatusForbidden)
				return
			}

			allowOrigin := ""
			if containsOSS3Wildcard(origins) {
				allowOrigin = "*"
			} else if middleware.IsOriginAllowed(origin, origins) {
				allowOrigin = origin
			} else {
				w.WriteHeader(http.StatusForbidden)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, DELETE, HEAD, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, Content-Length, Content-Encoding, "+
					"amz-sdk-invocation-id, amz-sdk-request, "+
					"x-amz-content-sha256, x-amz-date, x-amz-security-token, x-amz-user-agent, "+
					"x-amz-sdk-checksum-algorithm, x-amz-decoded-content-length, x-amz-trailer, "+
					"x-amz-checksum-crc32, x-amz-checksum-crc32c, x-amz-checksum-crc64nvme, "+
					"x-amz-checksum-sha1, x-amz-checksum-sha256, x-amz-checksum-sha512, x-amz-checksum-md5, "+
					"x-amz-acl, x-amz-copy-source, x-amz-meta-*, "+
					"x-amz-storage-class, x-amz-tagging, "+
					"x-amz-server-side-encryption, x-amz-website-redirect-location")
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
