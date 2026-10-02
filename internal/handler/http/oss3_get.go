package httphandler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleGetObject handles GET /{bucket}/{key} — download object.
func (h *OSS3Handler) handleGetObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.GetObject",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	// Parse Range header for partial content
	rangeHeader := r.Header.Get("Range")
	start, end, hasRange, err := parseRangeHeader(rangeHeader)
	if err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRange, r.URL.Path, "")
		return
	}

	// Resolve suffix range (e.g. "bytes=-500") to absolute byte positions.
	// parseRangeHeader returns start < 0 for suffix ranges; we need the file
	// size to convert to (fileSize - suffixLen, fileSize - 1).
	if hasRange && start < 0 {
		meta, headErr := h.oss3UC.Backend().HeadObject(r.Context(), bucket, key)
		if headErr != nil {
			rtcoss3.WriteS3Error(w, mapBackendError(headErr), r.URL.Path, "")
			return
		}
		suffixLen := -start
		if suffixLen >= meta.Size {
			// Suffix larger than file — return entire file per RFC 7233 §2.1
			start = 0
		} else {
			start = meta.Size - suffixLen
		}
		end = meta.Size - 1
	}

	var obj io.ReadCloser
	var meta rtcoss3.ObjectMeta

	if hasRange {
		obj, meta, err = h.oss3UC.Backend().GetObjectRange(r.Context(), bucket, key, start, end)
	} else {
		obj, meta, err = h.oss3UC.Backend().GetObject(r.Context(), bucket, key)
	}
	if err != nil {
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}
	defer func() {
		// H5: Add timeout protection for Close() to prevent blocking on context cancellation.
		// Use a separate context with timeout since the original context may be cancelled.
		//
		// NOTE: The goroutine below may outlive the request context if Close() is slow.
		// We use context.Background() for logging inside the goroutine for this reason.
		// If Close() hangs permanently the goroutine leaks until the underlying TCP
		// connection times out — this is an accepted trade-off to avoid blocking the
		// response. The MinIO client's Transport timeouts (ResponseHeaderTimeout: 60s,
		// IdleConnTimeout: 90s) bound the worst-case leak duration.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), objectCloseTimeout)
		defer closeCancel()

		closeDone := make(chan struct{})
		logger.SafeGo("oss3-object-close", func() {
			// Use Background context — r.Context() may be cancelled when the client
			// disconnects, which would cause the logger to drop the Warn message.
			bgCtx := context.Background()
			if closeErr := obj.Close(); closeErr != nil {
				logger.Warn(bgCtx, "failed to close object body",
					zap.String("bucket", bucket),
					zap.String("key", key),
					zap.Error(closeErr))
			}
			close(closeDone)
		})

		select {
		case <-closeDone:
			// Close completed normally
		case <-closeCtx.Done():
			// Close timed out — log warning but don't block response.
			// The orphaned goroutine will finish when the underlying connection closes.
			logger.Warn(context.Background(), "object body close timed out",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Duration("timeout", objectCloseTimeout))
		}
	}()

	// Set response headers
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))

	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// Stream object body with context awareness
	if _, err := io.Copy(w, obj); err != nil {
		// Check if context was cancelled (client disconnected)
		if r.Context().Err() != nil {
			logger.Info(r.Context(), "client disconnected during download",
				zap.String("bucket", bucket),
				zap.String("key", key))
		} else {
			logger.Warn(r.Context(), "failed to stream object body to client",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Error(err))
		}
	}
}

// parseRangeHeader parses HTTP Range header per RFC 7233.
// Supported forms:
//
//	"bytes=START-END"   — closed range (both bounds inclusive)
//	"bytes=START-"      — open-ended range (from START to end of file)
//	"bytes=-SUFFIX"     — suffix range (last SUFFIX bytes)
//
// Returns an error for malformed or semantically invalid ranges such as:
//   - wrong unit (anything other than "bytes=")
//   - more than one dash (e.g. "bytes=1-2-3")
//   - suffix length of 0 (e.g. "bytes=-0") — RFC 7233 §2.1 requires suffix-length >= 1
//   - start > end when both are specified
func parseRangeHeader(rangeHeader string) (start, end int64, hasRange bool, err error) {
	if rangeHeader == "" {
		return 0, 0, false, nil
	}

	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false, fmt.Errorf("invalid range format")
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	// Reject multiple dashes (e.g. "bytes=1-2-3"). A valid range spec contains
	// exactly one '-' separator.
	if strings.Count(rangeSpec, "-") != 1 {
		return 0, 0, false, fmt.Errorf("invalid range format")
	}

	dashIdx := strings.Index(rangeSpec, "-")
	left := rangeSpec[:dashIdx]
	right := rangeSpec[dashIdx+1:]

	if left == "" {
		// Suffix range: "-500" means last 500 bytes.
		// RFC 7233 §2.1: suffix-length = 1*DIGIT (must be >= 1).
		if right == "" {
			// "bytes=-" is invalid (no suffix length).
			return 0, 0, false, fmt.Errorf("invalid suffix range")
		}
		suffixLen, err := strconv.ParseInt(right, 10, 64)
		if err != nil {
			return 0, 0, false, err
		}
		if suffixLen <= 0 {
			// Reject "-0" and negative values.
			return 0, 0, false, fmt.Errorf("suffix length must be positive")
		}
		// Negative offset from end — resolved against file size in handleGetObject.
		return -suffixLen, -1, true, nil
	}

	start, err = strconv.ParseInt(left, 10, 64)
	if err != nil {
		return 0, 0, false, err
	}
	if start < 0 {
		return 0, 0, false, fmt.Errorf("invalid range values")
	}

	if right == "" {
		// Open-ended range: "500-" means from byte 500 to end of file.
		end = -1
	} else {
		end, err = strconv.ParseInt(right, 10, 64)
		if err != nil {
			return 0, 0, false, err
		}
		if end < start {
			return 0, 0, false, fmt.Errorf("invalid range values")
		}
	}

	return start, end, true, nil
}

// handleHeadObject handles HEAD /{bucket}/{key} — get object metadata.
func (h *OSS3Handler) handleHeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.HeadObject",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	// Get object metadata from backend
	meta, err := h.oss3UC.Backend().HeadObject(r.Context(), bucket, key)
	if err != nil {
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Set response headers
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}
