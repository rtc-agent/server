package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// contextKey is an unexported type to prevent collisions with context keys
// defined in other packages.
type contextKey string

const (
	// RequestIDHeader is the HTTP header name for request tracing.
	RequestIDHeader = "X-Request-ID"

	// requestIDContextKey is the context key for storing request ID.
	requestIDContextKey contextKey = "request_id"
)

// RequestID middleware generates or propagates a unique request ID for tracing.
//
// If the incoming request has an X-Request-ID header, it is reused.
// Otherwise, a new UUID v4 is generated.
// The request ID is:
//   - Stored in the request context under the "request_id" key
//   - Added to the response as X-Request-ID header
//   - Available for logging and distributed tracing
//
// This middleware should be placed early in the middleware chain (after Recovery)
// so that all subsequent middleware and handlers can access the request ID.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reuse existing request ID from header, or generate new one.
		reqID := r.Header.Get(RequestIDHeader)
		if reqID == "" {
			reqID = uuid.New().String()
		}

		// Store in context for handlers and logging.
		ctx := context.WithValue(r.Context(), requestIDContextKey, reqID)

		// Add to response headers.
		w.Header().Set(RequestIDHeader, reqID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID retrieves the request ID from the context.
// Returns an empty string if no request ID is present.
func GetRequestID(ctx context.Context) string {
	if reqID, ok := ctx.Value(requestIDContextKey).(string); ok {
		return reqID
	}
	return ""
}
