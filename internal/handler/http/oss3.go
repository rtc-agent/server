package httphandler

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// OSS3 handler constants.
const (
	// objectCloseTimeout is the maximum time to wait for an object body to close.
	// Prevents blocking the response if Close() hangs.
	objectCloseTimeout = 5 * time.Second

	// maxXMLRequestBodySize is the maximum XML body size for batch delete
	// and complete multipart requests. S3 spec allows up to 1000 keys per request;
	// typical XML is ~50KB. 1 MB provides ample headroom.
	maxXMLRequestBodySize = 1 << 20 // 1 MB

	// defaultMaxKeys is the default maximum number of keys returned by ListObjects.
	defaultMaxKeys = 1000

	// maxListKeys is the absolute maximum for ListObjects max-keys parameter.
	maxListKeys = 1000
)

// OSS3Handler handles S3-compatible object storage operations.
type OSS3Handler struct {
	oss3UC           *usecase.OSS3Usecase
	bucket           string // configured bucket name (e.g., "rtc-agent")
	maxFileSizeBytes int64  // maximum allowed single file size (Bug #7 fix)
	multipart        *OSS3MultipartHandler
}

// NewOSS3Handler creates a new OSS3 handler.
func NewOSS3Handler(oss3UC *usecase.OSS3Usecase, bucket string, maxFileSizeBytes int64) *OSS3Handler {
	return &OSS3Handler{
		oss3UC:           oss3UC,
		bucket:           bucket,
		maxFileSizeBytes: maxFileSizeBytes,
		multipart:        NewOSS3MultipartHandler(oss3UC, bucket),
	}
}

// OSS3Usecase returns the underlying usecase (for middleware wiring).
func (h *OSS3Handler) OSS3Usecase() *usecase.OSS3Usecase {
	return h.oss3UC
}

// ServeHTTP routes S3 requests to the appropriate handler.
func (h *OSS3Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Bug #7 fix: Early size check for PUT requests before any validation.
	// This fails fast for oversized uploads before expensive operations like auth.
	if r.Method == http.MethodPut && r.ContentLength > 0 && h.maxFileSizeBytes > 0 {
		// Skip copy operations (they don't have a request body)
		if r.Header.Get("X-Amz-Copy-Source") == "" {
			if r.ContentLength > h.maxFileSizeBytes {
				rtcoss3.WriteS3Error(w, rtcoss3.ErrEntityTooLarge, r.URL.Path, "")
				return
			}
		}
	}

	// Extract and validate bucket and key from path
	bucket, key, ok := h.validateS3Request(w, r)
	if !ok {
		return
	}

	// Store parsed path in context so downstream handlers (e.g., multipart)
	// don't need to re-parse it. This eliminates redundant parseS3Path calls.
	parsedPath := &contextx.OSS3ParsedPath{Bucket: bucket, Key: key}
	r = r.WithContext(contextx.WithOSS3ParsedPath(r.Context(), parsedPath))

	// Extract user ID from context (set by SigV4 middleware)
	userID := contextx.GetOSS3UserID(r.Context())
	if userID == "" {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Validate key format (skip for ListObjects which has empty key)
	if key != "" {
		if err := rtcoss3.ValidateKey(key, userID); err != nil {
			rtcoss3.WriteS3Error(w, err, r.URL.Path, "")
			return
		}
	}

	// Check if this is a multipart operation
	if isMultipartRequest(r) {
		h.multipart.ServeHTTP(w, r)
		return
	}

	// Route by HTTP method for basic operations
	switch r.Method {
	case http.MethodPut:
		h.handlePutObject(w, r, bucket, key)
	case http.MethodGet:
		if key == "" {
			h.handleListObjects(w, r, bucket)
		} else {
			h.handleGetObject(w, r, bucket, key)
		}
	case http.MethodDelete:
		h.handleDeleteObject(w, r, bucket, key)
	case http.MethodHead:
		h.handleHeadObject(w, r, bucket, key)
	case http.MethodPost:
		// POST /{bucket}?delete= — batch delete objects (H4)
		if r.URL.Query().Has("delete") && key == "" {
			h.handleDeleteObjects(w, r, bucket)
		} else {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
		}
	default:
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
	}
}

// validateS3Request extracts bucket and key from the S3 path and validates the bucket.
// Returns (bucket, key, true) on success, or writes an error and returns (..., false) on failure.
func (h *OSS3Handler) validateS3Request(w http.ResponseWriter, r *http.Request) (bucket, key string, ok bool) {
	bucket, key, ok = parseS3Path(r.URL.Path)
	if !ok {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidURI, r.URL.Path, "")
		return "", "", false
	}

	if bucket != h.bucket {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrBucketNotFound, r.URL.Path, "")
		return "", "", false
	}

	return bucket, key, true
}

// isMultipartRequest checks if the request is a multipart upload operation.
func isMultipartRequest(r *http.Request) bool {
	q := r.URL.Query()
	return q.Has("uploads") || q.Has("uploadId")
}

// parseS3Path extracts bucket and key from S3 path-style URL.
// Format: /s3/{bucket}/{key} or /s3/{bucket}
//
// Security: Rejects path traversal attempts (e.g., "../" sequences) to prevent
// escaping the user's namespace. This is a defense-in-depth measure; the key
// validation layer (rtcoss3.ValidateKey) also enforces strict format.
//
// Note: net/http automatically URL-decodes r.URL.Path before passing to handlers,
// so "%2e%2e" becomes ".." by the time we see it. However, X-Amz-Copy-Source
// header values are NOT auto-decoded, so we also check for URL-encoded variants.
func parseS3Path(path string) (bucket, key string, ok bool) {
	// Remove /s3 prefix
	path = strings.TrimPrefix(path, "/s3")
	// Remove leading slash
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", "", false
	}

	// Bug #10 fix: Check for URL-encoded path traversal variants.
	// net/http decodes r.URL.Path but not header values like X-Amz-Copy-Source.
	cleanedPath := filepath.Clean(path)
	if strings.HasPrefix(cleanedPath, "..") || strings.Contains(cleanedPath, "/../") {
		return "", "", false
	}
	if strings.Contains(path, "%2e%2e") || strings.Contains(path, "%2E%2E") ||
		strings.Contains(path, "%2e%2E") || strings.Contains(path, "%2E%2e") {
		return "", "", false
	}

	// Reject path traversal attempts (defense in depth).
	// net/http already decodes URL-encoded characters in r.URL.Path,
	// so we only need to check for literal ".." path segments.
	// We reject "/../", "/.." at end, ".." at start, and bare ".." (entire path).
	// This allows legitimate filenames containing ".." as a substring
	// (e.g., "my..file.txt") while blocking actual traversal.
	if path == ".." ||
		strings.HasPrefix(path, "../") ||
		strings.HasSuffix(path, "/..") ||
		strings.Contains(path, "/../") {
		return "", "", false
	}

	parts := strings.SplitN(path, "/", 2)
	bucket = parts[0]
	if len(parts) > 1 {
		key = parts[1]
		// Reject suspicious patterns in key
		if strings.Contains(key, "/..") || strings.HasSuffix(key, "/.") {
			return "", "", false
		}
	}
	return bucket, key, true
}

// isQuotaExceededError checks if the error is a quota-exceeded condition.
//
// Detection uses errors.Is() against the rtcoss3.ErrQuotaExceeded sentinel
// which is wrapped by the usecase layer.
func isQuotaExceededError(err error) bool {
	return errors.Is(err, rtcoss3.ErrQuotaExceeded)
}

// mapBackendError maps backend sentinel errors to S3-compatible error responses.
//
// The MinIO backend wraps domain sentinel errors (ErrBackendKeyNotFound,
// ErrBackendAccessDenied, etc.) so that they can be detected here with
// errors.Is() without relying on fragile string matching.
func mapBackendError(err error) *rtcoss3.S3Error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, rtcoss3.ErrBackendKeyNotFound):
		return rtcoss3.ErrKeyNotFound
	case errors.Is(err, rtcoss3.ErrBackendAccessDenied):
		return rtcoss3.ErrAccessDenied
	case errors.Is(err, rtcoss3.ErrBackendBucketNotFound):
		return rtcoss3.ErrBucketNotFound
	case errors.Is(err, rtcoss3.ErrBackendInsufficientStorage):
		return rtcoss3.ErrInsufficientStorage
	default:
		return rtcoss3.ErrInternalError
	}
}
