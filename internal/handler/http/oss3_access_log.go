package httphandler

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// AccessLogMiddleware logs S3 requests with structured fields.
type AccessLogMiddleware struct {
	oss3UC *usecase.OSS3Usecase
	next   http.Handler
}

// NewAccessLogMiddleware creates a new access log middleware.
func NewAccessLogMiddleware(oss3UC *usecase.OSS3Usecase, next http.Handler) *AccessLogMiddleware {
	return &AccessLogMiddleware{
		oss3UC: oss3UC,
		next:   next,
	}
}

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter
	statusCode   int
	responseSize int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.responseSize += n
	return n, err
}

// Flush implements http.Flusher for SSE and streaming support.
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack implements http.Hijacker for WebSocket support.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// contextEnricher is a mutable container to capture context values set by downstream handlers.
// This is needed because Go's http.Request is immutable, and when SigV4 middleware calls
// r.WithContext(ctx), it creates a new request that AccessLog cannot access directly.
type contextEnricher struct {
	userID    string
	requestID string
}

// ServeHTTP implements the http.Handler interface.
func (m *AccessLogMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Create a mutable container to capture context values from downstream handlers
	enricher := &contextEnricher{}

	// Wrap response writer to capture status and size
	rw := &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}

	// Extract S3-specific fields (these don't change)
	bucket, key, _ := parseS3Path(r.URL.Path)
	operation := extractOperation(r)

	// Wrap the request to capture context enrichment from SigV4 middleware
	originalContext := r.Context()
	wrappedReq := r.WithContext(&contextCapturer{
		Context:  originalContext,
		enricher: enricher,
	})

	// Call next handler
	m.next.ServeHTTP(rw, wrappedReq)

	// Calculate duration
	duration := time.Since(start)

	// Use captured user_id if available, otherwise fall back to empty string
	userID := enricher.userID
	requestID := enricher.requestID

	// Log structured access log
	logger.Info(originalContext, "[oss3.HTTP] request completed",
		zap.String("user_id", userID),
		zap.String("request_id", requestID),
		zap.String("bucket", bucket),
		zap.String("key", key),
		zap.String("operation", operation),
		zap.String("method", r.Method),
		zap.Int("status", rw.statusCode),
		zap.Int64("duration_ms", duration.Milliseconds()),
		zap.Int("response_size", rw.responseSize),
		zap.String("user_agent", r.UserAgent()),
		zap.Int64("content_length", r.ContentLength),
		zap.String("remote_addr", r.RemoteAddr),
		zap.String("referer", r.Referer()),
		zap.String("range", r.Header.Get("Range")),
		zap.String("copy_source", r.Header.Get("X-Amz-Copy-Source")),
		zap.String("etag", rw.Header().Get("ETag")),
	)
}

// contextCapturer wraps a context and captures specific values set by downstream handlers.
type contextCapturer struct {
	context.Context
	enricher *contextEnricher
}

// Value intercepts context.Value calls and captures user_id and request_id.
func (c *contextCapturer) Value(key interface{}) interface{} {
	val := c.Context.Value(key)
	// Capture specific keys when they are set by downstream handlers
	switch key {
	case ContextKeyUserID:
		if userID, ok := val.(string); ok {
			c.enricher.userID = userID
		}
	case ContextKeyRequestID:
		if requestID, ok := val.(string); ok {
			c.enricher.requestID = requestID
		}
	}
	return val
}

// extractOperation extracts the S3 operation name from the request.
// MEDIUM-21 fix: Caches the result in request context to avoid repeated parsing
// when called by both AccessLog and Metrics middlewares.
func extractOperation(r *http.Request) string {
	// Check cache first
	if cached, ok := r.Context().Value(ContextKeyOperation).(string); ok {
		return cached
	}

	_, key, _ := parseS3Path(r.URL.Path)
	q := r.URL.Query()

	var op string
	// Multipart operations
	if q.Has("uploads") && r.Method == http.MethodPost {
		op = "CreateMultipartUpload"
	} else if q.Has("uploadId") {
		switch r.Method {
		case http.MethodPut:
			op = "UploadPart"
		case http.MethodPost:
			op = "CompleteMultipartUpload"
		case http.MethodDelete:
			op = "AbortMultipartUpload"
		case http.MethodGet:
			op = "ListParts"
		}
	}

	// Basic operations
	if op == "" {
		if key == "" {
			if r.Method == http.MethodGet {
				op = "ListObjects"
			}
		}
	}

	if op == "" {
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				op = "CopyObject"
			} else {
				op = "PutObject"
			}
		case http.MethodGet:
			op = "GetObject"
		case http.MethodDelete:
			op = "DeleteObject"
		case http.MethodHead:
			op = "HeadObject"
		default:
			op = "Unknown"
		}
	}

	// Cache in context for subsequent calls
	*r = *r.WithContext(context.WithValue(r.Context(), ContextKeyOperation, op))
	return op
}
