package httphandler

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// RequestIDMiddleware generates and injects a request ID into the context.
// M5: Ensures all S3 requests have a unique ID for tracing and error reporting.
// If X-Amz-Request-Id header is present, uses it; otherwise generates a new UUID.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Amz-Request-Id")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		// Inject request ID into context via contextx (single source of truth)
		ctx := contextx.WithOSS3RequestID(r.Context(), requestID)
		r = r.WithContext(ctx)

		// Also set response header for client correlation
		w.Header().Set("X-Amz-Request-Id", requestID)

		next.ServeHTTP(w, r)
	})
}

// BusinessRestrictionMiddleware enforces business rules on S3 requests.
type BusinessRestrictionMiddleware struct {
	oss3UC *usecase.OSS3Usecase
	bucket string
	next   http.Handler
}

// NewBusinessRestrictionMiddleware creates a new business restriction middleware.
func NewBusinessRestrictionMiddleware(oss3UC *usecase.OSS3Usecase, bucket string, next http.Handler) *BusinessRestrictionMiddleware {
	return &BusinessRestrictionMiddleware{
		oss3UC: oss3UC,
		bucket: bucket,
		next:   next,
	}
}

// ServeHTTP implements the http.Handler interface.
// MEDIUM-14/19 fix: Removed duplicate path parsing, bucket validation, and path
// permission checks — the handler (ServeHTTP in oss3.go) already performs these.
// This middleware now only verifies that the user ID was set by SigV4.
func (m *BusinessRestrictionMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract user ID from context (set by SigV4 middleware) via contextx
	userID := contextx.GetOSS3UserID(r.Context())
	if userID == "" {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Call next handler
	m.next.ServeHTTP(w, r)
}

// RateLimitMiddleware enforces per-user rate limits on S3 requests.
// Consolidates CheckRateLimitOrReject calls that were previously scattered
// across all handler methods (10 occurrences in oss3.go and oss3_multipart.go).
type RateLimitMiddleware struct {
	oss3UC *usecase.OSS3Usecase
	next   http.Handler
}

// NewRateLimitMiddleware creates a new rate limit middleware.
func NewRateLimitMiddleware(oss3UC *usecase.OSS3Usecase, next http.Handler) *RateLimitMiddleware {
	return &RateLimitMiddleware{
		oss3UC: oss3UC,
		next:   next,
	}
}

// ServeHTTP implements the http.Handler interface.
// Checks the per-user rate limit before forwarding to the next handler.
// Returns 429 SlowDown if the limit is exceeded.
func (m *RateLimitMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID := contextx.GetOSS3UserID(r.Context())
	requestID := contextx.GetOSS3RequestID(r.Context())

	if err := m.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	m.next.ServeHTTP(w, r)
}

// hasPathPermission checks if the user has permission to access the given key.
// Users can only access paths under user-{user_id}/.
// NOTE: Currently only used in tests; production path validation is done via
// rtcoss3.ValidateKey in the handler layer. Retained for test coverage and
// as a standalone utility for future middleware use.
func hasPathPermission(userID, key string) bool {
	expectedPrefix := "user-" + userID + "/"
	return strings.HasPrefix(key, expectedPrefix) || key == ""
}

// ExtractUserIDFromContext is a helper to extract user ID from context.
// Delegates to contextx.GetOSS3UserID — the single source of truth for OSS3 context keys.
func ExtractUserIDFromContext(ctx context.Context) string {
	return contextx.GetOSS3UserID(ctx)
}

// ExtractRequestIDFromContext is a helper to extract request ID from context.
// Delegates to contextx.GetOSS3RequestID — the single source of truth for OSS3 context keys.
func ExtractRequestIDFromContext(ctx context.Context) string {
	return contextx.GetOSS3RequestID(ctx)
}
