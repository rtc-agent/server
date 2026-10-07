// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// requestIDHeader is the HTTP header name for request tracing.
	requestIDHeader = "X-Request-ID"
	// requestIDContextKey is the context key for storing request ID.
	requestIDContextKey = "request_id"
)

// AdminRequestIDMiddleware generates or propagates a unique request ID for tracing.
//
// If the incoming request has an X-Request-ID header, it is reused.
// Otherwise, a new UUID v4 is generated.
// The request ID is:
//   - Stored in gin.Context under "request_id" key
//   - Added to the response as X-Request-ID header
//   - Available for logging and distributed tracing
func AdminRequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Reuse existing request ID from header, or generate new one
		reqID := c.GetHeader(requestIDHeader)
		if reqID == "" {
			reqID = uuid.New().String()
		}

		// Store in context for handlers and logging
		c.Set(requestIDContextKey, reqID)

		// Add to response headers
		c.Header(requestIDHeader, reqID)

		c.Next()
	}
}
