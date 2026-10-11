package httphandler

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"time"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// OSS3MultipartHandler handles S3-compatible multipart upload operations.
type OSS3MultipartHandler struct {
	oss3UC           *usecase.OSS3Usecase
	bucket           string
	maxFileSizeBytes int64 // maximum allowed file size (P1-6 fix: for instant upload validation)
}

// NewOSS3MultipartHandler creates a new multipart upload handler.
func NewOSS3MultipartHandler(oss3UC *usecase.OSS3Usecase, bucket string, maxFileSizeBytes int64) *OSS3MultipartHandler {
	return &OSS3MultipartHandler{
		oss3UC:           oss3UC,
		bucket:           bucket,
		maxFileSizeBytes: maxFileSizeBytes,
	}
}

// validateUploadOwnership verifies that the multipart upload exists and belongs to the
// requesting user. Writes an S3 error response and returns false if validation fails.
func (h *OSS3MultipartHandler) validateUploadOwnership(
	w http.ResponseWriter, r *http.Request, userID, uploadID string,
) bool {
	_, err := h.oss3UC.ValidateUploadOwnership(r.Context(), userID, uploadID)
	if err != nil {
		if errors.Is(err, usecase.ErrUploadNotFound) {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrUploadNotFound, r.URL.Path, "")
		} else {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		}
		return false
	}
	return true
}

// handleCreateMultipartUpload handles POST /{bucket}/{key}?uploads
func (h *OSS3MultipartHandler) handleCreateMultipartUpload(
	w http.ResponseWriter, r *http.Request, bucket, key string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(
		r.Context(), "oss3.create_multipart_upload",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Check concurrent upload limit
	if err := h.oss3UC.CheckConcurrentUploadLimit(r.Context(), userID); err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMaxUploadsExceeded, r.URL.Path, "")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Validate Content-Type against whitelist (image/* and text/* only)
	if !rtcoss3.IsAllowedContentType(contentType) {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrUnsupportedContentType, r.URL.Path, "")
		return
	}

	// Create multipart upload in backend
	result, err := h.oss3UC.Backend().CreateMultipartUpload(r.Context(), bucket, key, contentType)
	if err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Create upload record in DB for tracking
	err = h.oss3UC.CreateMultipartUploadRecord(
		r.Context(), userID, bucket, key, result.UploadID)
	if err != nil {
		// P1-7 fix: DB failed but backend upload created — start async retry to prevent orphaned upload
		logger.Warn(r.Context(), "failed to create multipart upload record, starting async retry",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.String("upload_id", result.UploadID),
			zap.Error(err))

		// Launch async retry with exponential backoff (1s, 2s, 4s)
		logger.SafeGo("oss3.multipart.upload_record_retry", func() {
			h.retryCreateMultipartUploadRecord(userID, bucket, key, result.UploadID)
		})
	}

	RecordMultipartUploadStart()

	// Return XML response per S3 spec
	type initiateResult struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}
	resp := initiateResult{
		Bucket:   bucket,
		Key:      key,
		UploadID: result.UploadID,
	}
	writeXMLResponse(w, http.StatusOK, resp)
}

// ServeHTTP routes multipart upload requests to the appropriate handler.
//
// NOTE: Bucket validation is intentionally delegated to the parent OSS3Handler.
// This handler is only called from OSS3Handler.ServeHTTP after bucket validation.
// The parsed bucket/key are passed via context (set by OSS3Handler.ServeHTTP)
// to avoid redundant parseS3Path calls.
func (h *OSS3MultipartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Retrieve parsed bucket/key from context (set by OSS3Handler.ServeHTTP).
	// Fall back to re-parsing if not present (defensive — should always be set).
	bucket, key := "", ""
	if pp, ok := contextx.GetOSS3ParsedPath(r.Context()); ok {
		bucket, key = pp.Bucket, pp.Key
	} else {
		var parsed bool
		bucket, key, parsed = parseS3Path(r.URL.Path)
		if !parsed {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidURI, r.URL.Path, "")
			return
		}
	}

	// Extract uploadId if present
	uploadID := r.URL.Query().Get("uploadId")

	// Route by HTTP method and query parameters
	switch r.Method {
	case http.MethodPost:
		if r.URL.Query().Has("uploads") {
			h.handleCreateMultipartUpload(w, r, bucket, key)
		} else if uploadID != "" {
			h.handleCompleteMultipartUpload(w, r, bucket, key, uploadID)
		} else {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	case http.MethodPut:
		if uploadID != "" {
			h.handleUploadPart(w, r, bucket, key, uploadID)
		} else {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	case http.MethodDelete:
		if uploadID != "" {
			h.handleAbortMultipartUpload(w, r, bucket, key, uploadID)
		} else {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	case http.MethodGet:
		if uploadID != "" {
			h.handleListParts(w, r, bucket, key, uploadID)
		} else {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	default:
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
	}
}

// retryCreateMultipartUploadRecord attempts to create a multipart upload record in DB
// with exponential backoff (1s, 2s, 4s). P1-7 fix: prevents orphaned uploads when
// MinIO succeeds but DB fails initially. If all retries fail, logs Error for orphan reconciler.
func (h *OSS3MultipartHandler) retryCreateMultipartUploadRecord(
	userID, bucket, key, uploadID string,
) {
	// Use background context since original request context may be cancelled
	ctx := context.Background()

	// Exponential backoff: 1s, 2s, 4s
	backoffDurations := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

	for attempt, delay := range backoffDurations {
		time.Sleep(delay)

		err := h.oss3UC.CreateMultipartUploadRecord(ctx, userID, bucket, key, uploadID)
		if err == nil {
			logger.Info(ctx, "async retry: multipart upload record created successfully",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.String("upload_id", uploadID),
				zap.Int("attempt", attempt+1))
			return
		}

		logger.Warn(ctx, "async retry: failed to create multipart upload record",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.String("upload_id", uploadID),
			zap.Int("attempt", attempt+1),
			zap.Error(err))
	}

	// All retries failed — log Error for orphan reconciler to handle
	logger.Error(ctx, "async retry exhausted: orphaned multipart upload in MinIO",
		zap.String("user_id", userID),
		zap.String("key", key),
		zap.String("upload_id", uploadID))
}
