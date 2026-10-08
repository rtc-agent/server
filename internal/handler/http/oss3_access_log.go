package httphandler

import (
	"net/http"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/rtc-agent/server/internal/infra/contextx"
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

// responseHeaderUserKey is the response header set by SigV4 middleware to
// communicate the authenticated user ID back to the access log middleware.
// This avoids the need for complex context capture patterns.
const responseHeaderUserKey = "X-Oss3-User-Id"

// ServeHTTP implements the http.Handler interface.
func (m *AccessLogMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Extract S3-specific fields (these don't change)
	bucket, key, _ := parseS3Path(r.URL.Path)
	operation := extractOperation(r)

	// Store the operation in context via contextx for potential downstream use
	originalContext := r.Context()
	ctxWithOperation := contextx.WithOSS3Operation(originalContext, operation)
	r = r.WithContext(ctxWithOperation)

	// Use httpsnoop to capture status code and bytes written while preserving
	// all ResponseWriter interfaces (Hijacker, Flusher, Pusher, etc.).
	metrics := httpsnoop.CaptureMetrics(http.HandlerFunc(m.next.ServeHTTP), w, r)

	// Calculate duration
	duration := time.Since(start)

	// Read user_id and request_id from response headers set by inner middleware.
	// SigV4 sets X-Oss3-User-Id; RequestIDMiddleware sets X-Amz-Request-Id.
	// This approach is simpler and more robust than context capture patterns.
	userID := w.Header().Get(responseHeaderUserKey)
	requestID := w.Header().Get("X-Amz-Request-Id")

	// Log structured access log
	logger.Info(originalContext, "[oss3.HTTP] request completed",
		zap.String("user_id", userID),
		zap.String("request_id", requestID),
		zap.String("bucket", bucket),
		zap.String("key", key),
		zap.String("operation", operation),
		zap.String("method", r.Method),
		zap.Int("status", metrics.Code),
		zap.String("duration", duration.String()), // LOW-08 fix: use string format instead of integer ms
		zap.Int64("response_size", metrics.Written),
		zap.String("user_agent", r.UserAgent()),
		zap.Int64("content_length", r.ContentLength),
		zap.String("remote_addr", r.RemoteAddr),
		zap.String("referer", r.Referer()),
		zap.String("range", r.Header.Get("Range")),
		zap.String("copy_source", r.Header.Get("X-Amz-Copy-Source")),
		zap.String("etag", w.Header().Get("ETag")),
	)
}

// extractOperation extracts the S3 operation name from the request.
// This is a pure function — no side effects on the request.
// The operation is computed once by AccessLogMiddleware and cached in context
// via contextx.WithOSS3Operation for potential future use by downstream handlers.
func extractOperation(r *http.Request) string {
	_, key, _ := parseS3Path(r.URL.Path)
	q := r.URL.Query()

	// Multipart operations
	if q.Has("uploads") && r.Method == http.MethodPost {
		return "CreateMultipartUpload"
	}
	if q.Has("uploadId") {
		switch r.Method {
		case http.MethodPut:
			return "UploadPart"
		case http.MethodPost:
			return "CompleteMultipartUpload"
		case http.MethodDelete:
			return "AbortMultipartUpload"
		case http.MethodGet:
			return "ListParts"
		}
	}

	// Basic operations
	if key == "" && r.Method == http.MethodGet {
		return "ListObjects"
	}

	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			return "CopyObject"
		}
		return "PutObject"
	case http.MethodGet:
		return "GetObject"
	case http.MethodDelete:
		return "DeleteObject"
	case http.MethodHead:
		return "HeadObject"
	default:
		return "Unknown"
	}
}
