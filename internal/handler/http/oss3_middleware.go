package httphandler

import (
	"context"
	"net/http"
	"strings"

	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

const (
	// ContextKeyUserID is the context key for user ID.
	ContextKeyUserID contextKey = "user_id"
	// ContextKeyRequestID is the context key for request ID.
	ContextKeyRequestID contextKey = "request_id"
)

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
func (m *BusinessRestrictionMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract bucket and key from path
	bucket, key, ok := parseS3Path(r.URL.Path)
	if !ok {
		WriteS3Error(w, rtcoss3.ErrInvalidURI, r.URL.Path, "")
		return
	}

	// Validate bucket
	if bucket != m.bucket {
		WriteS3Error(w, rtcoss3.ErrNoSuchBucket, r.URL.Path, "")
		return
	}

	// Extract user ID from context (set by SigV4 middleware)
	userID := r.Context().Value(ContextKeyUserID)
	if userID == nil {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Check path permission: user can only access user-{user_id}/* paths
	if !hasPathPermission(userIDStr, key) {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Call next handler
	m.next.ServeHTTP(w, r)
}

// hasPathPermission checks if the user has permission to access the given key.
// Users can only access paths under user-{user_id}/.
func hasPathPermission(userID, key string) bool {
	expectedPrefix := "user-" + userID + "/"
	return strings.HasPrefix(key, expectedPrefix) || key == ""
}

// ExtractUserIDFromContext is a helper to extract user ID from context.
func ExtractUserIDFromContext(ctx context.Context) string {
	userID := ctx.Value(ContextKeyUserID)
	if userID == nil {
		return ""
	}
	userIDStr, ok := userID.(string)
	if !ok {
		return ""
	}
	return userIDStr
}

// ExtractRequestIDFromContext is a helper to extract request ID from context.
func ExtractRequestIDFromContext(ctx context.Context) string {
	requestID := ctx.Value(ContextKeyRequestID)
	if requestID == nil {
		return ""
	}
	requestIDStr, ok := requestID.(string)
	if !ok {
		return ""
	}
	return requestIDStr
}
