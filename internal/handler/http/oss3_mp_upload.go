package httphandler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleUploadPart handles PUT /{bucket}/{key}?partNumber={n}&uploadId={id}
func (h *OSS3MultipartHandler) handleUploadPart(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.upload_part",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("oss.upload.id", uploadID),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Verify upload exists and belongs to this user
	if !h.validateUploadOwnership(w, r, userID, uploadID) {
		return
	}

	// Parse part number
	partNumberStr := r.URL.Query().Get("partNumber")
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidPartNumber, r.URL.Path, "")
		return
	}

	// Bug #4 fix: Acquire distributed lock (keyed by uploadID) to serialize
	// concurrent UploadPart requests for the same multipart upload.
	// Without this lock, two concurrent requests for the same partNumber can
	// both pass the ListParts check (TOCTOU race), resulting in the second
	// overwriting the first while both reserving quota — double billing.
	//
	// Lock granularity: per uploadID (not global), so different uploads proceed
	// in parallel. TTL of 30s covers the ListParts + UploadPart sequence.
	const uploadPartLockTTL = 30 * time.Second
	lockHolder, acquired, lockErr := h.oss3UC.AcquireUploadPartLock(r.Context(), uploadID, uploadPartLockTTL)
	if lockErr != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(lockErr)
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}
	if !acquired {
		// Another request holds the lock for this upload — tell client to retry.
		rtcoss3.WriteS3Error(w, rtcoss3.ErrLockAcquireFailed, r.URL.Path, "")
		return
	}
	defer h.oss3UC.ReleaseUploadPartLock(r.Context(), uploadID, lockHolder)

	// Check if part with this number already exists to prevent data overwrite
	existingParts, err := h.oss3UC.Backend().ListParts(r.Context(), bucket, key, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}
	for _, p := range existingParts {
		if p.PartNumber == partNumber {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidPart, r.URL.Path, "")
			return
		}
	}

	contentLength := r.ContentLength
	if contentLength < 0 {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMissingContentLength, r.URL.Path, "")
		return
	}

	// Bug #9 fix: Limit request body to the declared Content-Length.
	// This prevents a malicious client from sending more data than declared
	// (and more than quota reserved), which would otherwise be buffered by
	// the backend uploader. Defense-in-depth alongside the early maxFileSizeBytes
	// check in OSS3Handler.ServeHTTP.
	r.Body = http.MaxBytesReader(w, r.Body, contentLength)

	// Check quota for this part.
	// Zero-byte parts skip quota reservation — they consume no storage.
	var quotaRequestID string
	if contentLength > 0 {
		qrid, err := h.oss3UC.CheckAndReserveQuota(r.Context(), userID, contentLength)
		if err != nil {
			writeQuotaReserveError(w, err, r.URL.Path)
			return
		}
		quotaRequestID = qrid
	}

	// Ensure quota is released on failure unless committed.
	// quotaCommitted tracks whether quota has been charged to the user.
	// If false at function exit, the defer releases the pending reservation.
	quotaCommitted := false
	// Bug #15 fix: Use WithoutCancel to preserve tracing context while avoiding cancellation.
	defer func() {
		if !quotaCommitted && quotaRequestID != "" {
			h.oss3UC.ReleaseQuota(context.WithoutCancel(r.Context()), userID, quotaRequestID)
		}
	}()

	// Upload part to backend
	etag, err := h.oss3UC.Backend().UploadPart(
		r.Context(), bucket, key, uploadID, partNumber, r.Body, contentLength)
	if err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Commit quota with retry after successful upload.
	// Skip for zero-byte parts — no quota was reserved.
	if quotaRequestID != "" {
		if err = commitQuotaWithRetry(r.Context(), h.oss3UC, userID, quotaRequestID, contentLength); err != nil {
			// Commit failed after all retries but upload succeeded — data lives in MinIO.
			// Do NOT call ReleaseQuota: the pending key will expire via TTL naturally.
			// Reconciliation will eventually sync the Redis counter with DB truth.
			logger.Error(r.Context(), "quota commit failed after retries; reconciliation will sync",
				zap.String("user_id", userID),
				zap.String("upload_id", uploadID),
				zap.Int("part_number", partNumber),
				zap.Int64("amount", contentLength),
				zap.Error(err))
			RecordOrphanedQuotaCommit()
		}
		quotaCommitted = true // Mark as committed regardless of success — avoid double-release
	} else {
		quotaCommitted = true
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}
