package httphandler

import (
	"context"
	"net/http"
	"strings"

	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// contextKey is a custom type for OSS3-specific context keys.
//
// NOTE: The project-wide convention (internal/infra/contextx) uses an unexported
// struct type `contextKey{ name string }` for keys. OSS3 intentionally uses a
// `string`-backed type here because:
//   - OSS3's user_id is a string (from the session token), not a uuid.UUID like
//     contextx's userIDKey. Mixing types would force lossy conversions.
//   - OSS3 keys are exported (ContextKeyUserID, etc.) for cross-package access
//     between the handler and middleware, whereas contextx keys are unexported
//     and accessed via getter functions.
//
// Keeping them separate avoids coupling OSS3 to the contextx package and
// preserves the distinct semantics of each key namespace.
type contextKey string

const (
	// ContextKeyUserID is the context key for user ID.
	ContextKeyUserID contextKey = "user_id"
	// ContextKeyRequestID is the context key for request ID.
	ContextKeyRequestID contextKey = "request_id"
	// ContextKeyOperation is the context key for cached S3 operation name.
	ContextKeyOperation contextKey = "operation"
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
// MEDIUM-14/19 fix: Removed duplicate path parsing, bucket validation, and path
// permission checks — the handler (ServeHTTP in oss3.go) already performs these.
// This middleware now only verifies that the user ID was set by SigV4.
func (m *BusinessRestrictionMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	// Call next handler
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
