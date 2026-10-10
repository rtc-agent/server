package httphandler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleInstantUploadResult processes the result of CheckInstantUpload.
// Returns true if the request was fully handled (caller should return),
// or false if the caller should continue with normal upload flow.
func (h *OSS3Handler) handleInstantUploadResult(
	w http.ResponseWriter, r *http.Request,
	bucket, key, userID string,
	instantResult *usecase.InstantUploadResult,
	span trace.Span,
) bool {
	if instantResult.Hit {
		if instantResult.ConsistencyViolation {
			return h.handleMinIORepair(w, r, bucket, key, userID, instantResult, span)
		}
		// Both DB and MinIO have the file → instant upload hit
		span.SetAttributes(attribute.String("oss.upload_source", "db_hit"))
		logger.Info(r.Context(), "instant upload: DB and MinIO hit",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Int64("size", instantResult.FileMeta.Size))
		RecordInstantUpload("db_hit")
		w.Header().Set("ETag", instantResult.FileMeta.ETag)
		w.WriteHeader(http.StatusOK)
		return true
	}

	if instantResult.ConsistencyViolation {
		// DB has record but MinIO missing → consistency violation, fall through to normal upload
		span.SetAttributes(attribute.Bool("db.consistency_violation", true))
		RecordConsistencyViolation(instantResult.ViolationType)
		logger.Warn(r.Context(), "instant upload: DB record exists but MinIO file missing, re-uploading",
			zap.String("user_id", userID),
			zap.String("key", key))
	}
	return false
}

// handleMinIORepair handles the case where MinIO has a file but DB is missing.
// It validates content-type and size before repairing the DB record.
// Returns true (request handled) in all cases.
func (h *OSS3Handler) handleMinIORepair(
	w http.ResponseWriter, r *http.Request,
	bucket, key, userID string,
	instantResult *usecase.InstantUploadResult,
	span trace.Span,
) bool {
	ctx := r.Context()

	// Validate content-type (prevents presigned URL bypass attacks)
	if !rtcoss3.IsAllowedContentType(instantResult.FileMeta.ContentType) {
		span.SetAttributes(attribute.String("oss.upload_source", "minio_repair_rejected"))
		span.SetStatus(codes.Error, "content_type_violation")
		logger.Warn(ctx, "instant upload: MinIO file has disallowed content-type, deleting",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.String("content_type", instantResult.FileMeta.ContentType))
		if delErr := h.oss3UC.Backend().DeleteObject(ctx, bucket, key); delErr != nil {
			logger.Error(ctx, "instant upload: failed to delete invalid MinIO file",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.Error(delErr))
		}
		rtcoss3.WriteS3Error(w, rtcoss3.ErrUnsupportedContentType, r.URL.Path, "")
		return true
	}

	// Validate file size
	if h.maxFileSizeBytes > 0 && instantResult.FileMeta.Size > h.maxFileSizeBytes {
		span.SetAttributes(attribute.String("oss.upload_source", "minio_repair_rejected"))
		span.SetStatus(codes.Error, "size_violation")
		logger.Warn(ctx, "instant upload: MinIO file exceeds max size, deleting",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Int64("size", instantResult.FileMeta.Size),
			zap.Int64("max_size", h.maxFileSizeBytes))
		if delErr := h.oss3UC.Backend().DeleteObject(ctx, bucket, key); delErr != nil {
			logger.Error(ctx, "instant upload: failed to delete oversized MinIO file",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.Error(delErr))
		}
		rtcoss3.WriteS3Error(w, rtcoss3.ErrEntityTooLarge, r.URL.Path, "")
		return true
	}

	// Repair DB record
	span.SetAttributes(attribute.String("oss.upload_source", "minio_repair"))
	RecordConsistencyViolation(instantResult.ViolationType)
	logger.Info(ctx, "instant upload: MinIO hit, repairing DB record",
		zap.String("user_id", userID),
		zap.String("key", key),
		zap.Int64("size", instantResult.FileMeta.Size))
	RecordInstantUpload("minio_repair")
	if repairErr := h.oss3UC.CreateFileRecord(ctx, instantResult.FileMeta); repairErr != nil {
		span.RecordError(repairErr)
		RecordInstantUploadRepairError()
		logger.Warn(ctx, "instant upload: DB repair failed",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Error(repairErr))
	}
	w.Header().Set("ETag", instantResult.FileMeta.ETag)
	w.WriteHeader(http.StatusOK)
	return true
}

// handlePutObject handles PUT /{bucket}/{key} — upload object.
//
// The function implements a compensation flow: quota reservation -> backend upload ->
// quota commit -> DB record. Helper functions handle instant upload checks and
// consistency repairs to keep this function readable.
func (h *OSS3Handler) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.put_object",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.Int64("http.request.body.size", r.ContentLength),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	// Check if this is a copy operation (x-amz-copy-source header)
	copySource := r.Header.Get("X-Amz-Copy-Source")
	if copySource != "" {
		// Validate copy source length (S3 spec: max 1024 chars)
		if len(copySource) > 1024 {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidCopySource, r.URL.Path, "")
			return
		}
		h.handleCopyObject(w, r, bucket, key, copySource)
		return
	}

	// Regular upload
	contentLength := r.ContentLength
	if contentLength < 0 {
		// Chunked transfer encoding not yet supported in Phase 1
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMissingContentLength, r.URL.Path, "")
		return
	}

	// Note: MaxFileSizeBytes check is done early in ServeHTTP before auth/validation.
	// This is defense-in-depth; the early check prevents wasted work on oversized uploads.

	// Limit request body to the declared Content-Length.
	// This prevents a malicious client from sending more data than declared
	// (and more than quota reserved), which would otherwise be buffered.
	r.Body = http.MaxBytesReader(w, r.Body, contentLength)

	userID := ExtractUserIDFromContext(r.Context())
	if userID == "" {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Instant upload check: Since the key contains MD5 (format: user-{userID}/{md5-hash}.{ext}),
	// the same key implies the same content. Check DB and MinIO consistency.
	// M2: Refactored to use CheckInstantUpload from usecase layer.
	//
	// Note: Concurrent uploads of the same key are safe due to content-addressing.
	// The upsert in Create() ensures eventual consistency. Race conditions only waste
	// bandwidth but don't corrupt data.
	instantResult, err := h.oss3UC.CheckInstantUpload(ctx, userID, bucket, key)
	if err != nil {
		// Database error — fail fast
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	if h.handleInstantUploadResult(w, r, bucket, key, userID, instantResult, span) {
		return
	}

	// Reserve quota; release on any subsequent failure unless committed.
	// Zero-byte uploads skip quota reservation — they consume no storage.
	var quotaRequestID string
	if contentLength > 0 {
		qrid, err := h.oss3UC.CheckAndReserveQuota(r.Context(), userID, contentLength)
		if err != nil {
			writeQuotaReserveError(w, err, r.URL.Path)
			return
		}
		quotaRequestID = qrid
	}
	// uploadSucceeded tracks whether data reached MinIO.
	// If false at function exit, the defer releases the pending reservation.
	// If true, quota is either committed or will be reconciled later — we must
	// NOT call ReleaseQuota because the pending key has already been consumed
	// by the (failed) commit attempt and the data lives in MinIO.
	uploadSucceeded := false
	// Bug #15 fix: Use WithoutCancel to preserve tracing context while avoiding cancellation.
	defer func() {
		if !uploadSucceeded && quotaRequestID != "" {
			h.oss3UC.ReleaseQuota(context.WithoutCancel(r.Context()), userID, quotaRequestID)
		}
	}()

	// Upload to backend with compensation
	contentType := r.Header.Get("Content-Type")

	// Validate Content-Type against whitelist (image/* and text/* only)
	if !rtcoss3.IsAllowedContentType(contentType) {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrUnsupportedContentType, r.URL.Path, "")
		return
	}

	etag, err := h.oss3UC.Backend().PutObject(
		r.Context(), bucket, key, r.Body, contentLength, contentType)
	if err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		// Check if the error is due to request body being too large
		if strings.Contains(err.Error(), "http: request body too large") {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrEntityTooLarge, r.URL.Path, "")
			return
		}
		RecordBackendError("PutObject", "upload_failed")
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return // uploadSucceeded=false -> defer releases quota
	}
	uploadSucceeded = true

	// Commit quota with retry (idempotent via SETNX marker).
	// Skip for zero-byte uploads — no quota was reserved.
	if quotaRequestID != "" {
		if err = commitQuotaWithRetry(r.Context(), h.oss3UC, userID, quotaRequestID, contentLength); err != nil {
			// Commit failed after all retries but upload succeeded — data lives in MinIO.
			// Do NOT call ReleaseQuota: the pending key will expire via TTL naturally.
			// Reconciliation will eventually sync the Redis counter with DB truth.
			logger.Error(r.Context(), "quota commit failed after retries; reconciliation will sync",
				zap.String("user_id", userID),
				zap.String("quota_request_id", quotaRequestID),
				zap.Int64("amount", contentLength),
				zap.Error(err))
			RecordOrphanedQuotaCommit()
		}
	}

	// Create file record in DB with compensation
	err = h.oss3UC.PutObjectWithComp(
		r.Context(), userID, bucket, key, contentLength, contentType, etag)
	if err != nil {
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// commitQuotaWithRetry retries CommitQuota on transient Redis errors.
// The idempotency marker inside CommitQuota (SETNX) prevents double-charging.
// Uses cenkalti/backoff for exponential backoff with jitter.
func commitQuotaWithRetry(
	ctx context.Context,
	oss3UC *usecase.OSS3Usecase,
	userID, quotaRequestID string,
	amount int64,
) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 50 * time.Millisecond
	bo.MaxInterval = 200 * time.Millisecond

	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		if err := oss3UC.CommitQuota(ctx, userID, quotaRequestID, amount); err != nil {
			if !isTransientRedisError(err) {
				return struct{}{}, backoff.Permanent(err)
			}
			RecordQuotaCommitRetry()
			return struct{}{}, err
		}
		return struct{}{}, nil
	},
		backoff.WithBackOff(bo),
		backoff.WithMaxTries(3),
	)
	return err
}

// isTransientRedisError reports whether the error is a transient Redis failure
// that is safe to retry (connection reset, I/O timeout, NOSCRIPT).
func isTransientRedisError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "NOSCRIPT")
}

// writeQuotaReserveError writes the appropriate S3 error response for a
// failed quota reservation. Quota-exceeded is mapped to the S3
// RequestQuotaExceeded code; all other errors become InternalError.
func writeQuotaReserveError(w http.ResponseWriter, err error, resource string) {
	if isQuotaExceededError(err) {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrRequestQuotaExceeded, resource, "")
	} else {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, resource, "")
	}
}
