package httphandler

import (
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

// ServeHTTP implements the http.Handler interface.
func (m *AccessLogMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Wrap response writer to capture status and size
	rw := &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}

	// Extract context fields
	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Extract S3-specific fields
	bucket, key, _ := parseS3Path(r.URL.Path)
	operation := extractOperation(r)

	// Call next handler
	m.next.ServeHTTP(rw, r)

	// Calculate duration
	duration := time.Since(start)

	// Log structured access log
	logger.Info(r.Context(), "[oss3.HTTP] request completed",
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
	)
}

// extractOperation extracts the S3 operation name from the request.
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
	if key == "" {
		if r.Method == http.MethodGet {
			return "ListObjects"
		}
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
	}

	return "Unknown"
}
