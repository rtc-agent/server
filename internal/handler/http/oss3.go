package httphandler

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// OSS3Handler handles S3-compatible object storage operations.
type OSS3Handler struct {
	oss3UC    *usecase.OSS3Usecase
	bucket    string // configured bucket name (e.g., "rtc-agent")
	multipart *OSS3MultipartHandler
}

// NewOSS3Handler creates a new OSS3 handler.
func NewOSS3Handler(oss3UC *usecase.OSS3Usecase, bucket string) *OSS3Handler {
	return &OSS3Handler{
		oss3UC:    oss3UC,
		bucket:    bucket,
		multipart: NewOSS3MultipartHandler(oss3UC, bucket),
	}
}

// OSS3Usecase returns the underlying usecase (for middleware wiring).
func (h *OSS3Handler) OSS3Usecase() *usecase.OSS3Usecase {
	return h.oss3UC
}

// ServeHTTP routes S3 requests to the appropriate handler.
func (h *OSS3Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract and validate bucket and key from path
	bucket, key, ok := h.validateS3Request(w, r)
	if !ok {
		return
	}

	// Extract user ID from context (set by SigV4 middleware)
	userID, ok := r.Context().Value(ContextKeyUserID).(string)
	if !ok || userID == "" {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Validate key format (skip for ListObjects which has empty key)
	if key != "" {
		if err := rtcoss3.ValidateKey(key, userID); err != nil {
			WriteS3Error(w, err, r.URL.Path, "")
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
			WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
		}
	default:
		WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
	}
}

// validateS3Request extracts bucket and key from the S3 path and validates the bucket.
// Returns (bucket, key, true) on success, or writes an error and returns (..., false) on failure.
func (h *OSS3Handler) validateS3Request(w http.ResponseWriter, r *http.Request) (bucket, key string, ok bool) {
	bucket, key, ok = parseS3Path(r.URL.Path)
	if !ok {
		WriteS3Error(w, rtcoss3.ErrInvalidURI, r.URL.Path, "")
		return "", "", false
	}

	if bucket != h.bucket {
		WriteS3Error(w, rtcoss3.ErrNoSuchBucket, r.URL.Path, "")
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
// so "%2e%2e" becomes ".." by the time we see it. We only need to check for
// literal ".." here.
func parseS3Path(path string) (bucket, key string, ok bool) {
	// Remove /s3 prefix
	path = strings.TrimPrefix(path, "/s3")
	// Remove leading slash
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", "", false
	}

	// Reject path traversal attempts (defense in depth).
	// net/http already decodes URL-encoded characters in r.URL.Path,
	// so we only need to check for literal ".." sequences.
	if strings.Contains(path, "..") {
		return "", "", false
	}

	parts := strings.SplitN(path, "/", 2)
	bucket = parts[0]
	if len(parts) > 1 {
		key = parts[1]
		// Additional validation: reject suspicious patterns in key
		if strings.Contains(key, "/..") || strings.HasSuffix(key, "/.") {
			return "", "", false
		}
	}
	return bucket, key, true
}

// handlePutObject handles PUT /{bucket}/{key} — upload object.
//
// The function is intentionally linear (not deeply decomposed) because it
// implements a compensation flow: quota reservation -> backend upload ->
// quota commit -> DB record. Splitting it would hide the data flow.
func (h *OSS3Handler) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.PutObject",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.Int64("content_length", r.ContentLength),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	// Check if this is a copy operation (x-amz-copy-source header)
	copySource := r.Header.Get("X-Amz-Copy-Source")
	if copySource != "" {
		// Validate copy source length (S3 spec: max 1024 chars)
		if len(copySource) > 1024 {
			WriteS3Error(w, rtcoss3.ErrInvalidCopySource, r.URL.Path, "")
			return
		}
		h.handleCopyObject(w, r, bucket, key, copySource)
		return
	}

	// Regular upload
	contentLength := r.ContentLength
	if contentLength < 0 {
		// Chunked transfer encoding not yet supported in Phase 1
		WriteS3Error(w, rtcoss3.ErrMissingContentLength, r.URL.Path, "")
		return
	}

	// Limit request body to the declared Content-Length.
	// This prevents a malicious client from sending more data than declared
	// (and more than quota reserved), which would otherwise be buffered.
	r.Body = http.MaxBytesReader(w, r.Body, contentLength)

	userID := ExtractUserIDFromContext(r.Context())
	if userID == "" {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Check rate limit before proceeding
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, ExtractRequestIDFromContext(r.Context())); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
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
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	if instantResult.Hit {
		if instantResult.ConsistencyViolation {
			// MinIO has file but DB missing → consistency repair
			span.SetAttributes(attribute.String("instant_upload", "minio_repair"))
			RecordConsistencyViolation(instantResult.ViolationType)
			logger.Info(ctx, "instant upload: MinIO hit, repairing DB record",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.Int64("size", instantResult.FileMeta.Size))
			RecordInstantUpload("minio_repair")
			if repairErr := h.oss3UC.CreateFileRecord(ctx, instantResult.FileMeta); repairErr != nil {
				// Repair failed, log but continue (MinIO file exists)
				span.RecordError(repairErr)
				RecordInstantUploadRepairError()
				logger.Warn(ctx, "instant upload: DB repair failed",
					zap.String("user_id", userID),
					zap.String("key", key),
					zap.Error(repairErr))
			}
		} else {
			// Both DB and MinIO have the file → instant upload hit
			span.SetAttributes(attribute.String("instant_upload", "db_hit"))
			logger.Info(ctx, "instant upload: DB and MinIO hit",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.Int64("size", instantResult.FileMeta.Size))
			RecordInstantUpload("db_hit")
		}
		w.Header().Set("ETag", instantResult.FileMeta.ETag)
		w.WriteHeader(http.StatusOK)
		return
	}

	if instantResult.ConsistencyViolation {
		// DB has record but MinIO missing → consistency violation, fall through to normal upload
		span.SetAttributes(attribute.Bool("db_consistency_violation", true))
		RecordConsistencyViolation(instantResult.ViolationType)
		logger.Warn(ctx, "instant upload: DB record exists but MinIO file missing, re-uploading",
			zap.String("user_id", userID),
			zap.String("key", key))
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
	defer func() {
		if !uploadSucceeded && quotaRequestID != "" {
			h.oss3UC.ReleaseQuota(r.Context(), userID, quotaRequestID)
		}
	}()

	// Upload to backend with compensation
	contentType := r.Header.Get("Content-Type")
	etag, err := h.oss3UC.Backend().PutObject(
		r.Context(), bucket, key, r.Body, contentLength, contentType)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		RecordBackendError("PutObject", err.Error())
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return // uploadSucceeded=false -> defer releases quota
	}
	uploadSucceeded = true

	// Commit quota with retry (idempotent via SETNX marker).
	// Skip for zero-byte uploads — no quota was reserved.
	if quotaRequestID != "" {
		if err = h.commitQuotaWithRetry(r.Context(), userID, quotaRequestID, contentLength); err != nil {
			// Commit failed after all retries but upload succeeded — data lives in MinIO.
			// Do NOT call ReleaseQuota: the pending key will expire via TTL naturally.
			// Reconciliation will eventually sync the Redis counter with DB truth.
			logger.Error(r.Context(), "quota commit failed after retries; reconciliation will sync",
				zap.String("user_id", userID),
				zap.String("quota_request_id", quotaRequestID),
				zap.Int64("amount", contentLength),
				zap.Error(err))
			RecordOrphanedQuotaCommit(userID)
		}
	}

	// Create file record in DB with compensation
	err = h.oss3UC.PutObjectWithComp(
		r.Context(), userID, bucket, key, contentLength, contentType, etag)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// commitQuotaWithRetry retries CommitQuota on transient Redis errors.
// The idempotency marker inside CommitQuota (SETNX) prevents double-charging.
// Retry policy: 3 attempts, linear backoff 50ms/100ms/200ms.
func (h *OSS3Handler) commitQuotaWithRetry(
	ctx context.Context, userID, quotaRequestID string, amount int64,
) error {
	const maxAttempts = 3
	delays := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := h.oss3UC.CommitQuota(ctx, userID, quotaRequestID, amount); err != nil {
			lastErr = err
			if !isTransientRedisError(err) {
				return err
			}
			RecordQuotaCommitRetry()
			if attempt < maxAttempts-1 {
				select {
				case <-time.After(delays[attempt]):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}
		return nil
	}
	return lastErr
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
		WriteS3Error(w, rtcoss3.ErrRequestQuotaExceeded, resource, "")
	} else {
		WriteS3Error(w, rtcoss3.ErrInternalError, resource, "")
	}
}

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

	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Check rate limit
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Parse Range header for partial content
	rangeHeader := r.Header.Get("Range")
	start, end, hasRange, err := parseRangeHeader(rangeHeader)
	if err != nil {
		WriteS3Error(w, rtcoss3.ErrInvalidRange, r.URL.Path, "")
		return
	}

	// Resolve suffix range (e.g. "bytes=-500") to absolute byte positions.
	// parseRangeHeader returns start < 0 for suffix ranges; we need the file
	// size to convert to (fileSize - suffixLen, fileSize - 1).
	if hasRange && start < 0 {
		meta, headErr := h.oss3UC.Backend().HeadObject(r.Context(), bucket, key)
		if headErr != nil {
			WriteS3Error(w, mapBackendError(headErr), r.URL.Path, "")
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
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}
	defer func() {
		// H5: Add timeout protection for Close() to prevent blocking on context cancellation.
		// Use a separate context with 5s timeout since the original context may be cancelled.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()

		closeDone := make(chan struct{})
		go func() {
			if closeErr := obj.Close(); closeErr != nil {
				logger.Warn(r.Context(), "failed to close object body",
					zap.String("bucket", bucket),
					zap.String("key", key),
					zap.Error(closeErr))
			}
			close(closeDone)
		}()

		select {
		case <-closeDone:
			// Close completed normally
		case <-closeCtx.Done():
			// Close timed out — log warning but don't block response
			logger.Warn(r.Context(), "object body close timed out after 5s",
				zap.String("bucket", bucket),
				zap.String("key", key))
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

// handleDeleteObject handles DELETE /{bucket}/{key} — delete object.
func (h *OSS3Handler) handleDeleteObject(
	w http.ResponseWriter, r *http.Request, bucket, key string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.DeleteObject",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Check rate limit
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Delete from backend
	if err := h.oss3UC.Backend().DeleteObject(r.Context(), bucket, key); err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete file record from DB with retry logic for compensation
	if err := h.oss3UC.DeleteFileRecord(r.Context(), userID, key); err != nil {
		// LOW-18 fix: DB failed but backend delete succeeded — orphaned record
		// Implement simple retry with exponential backoff
		logger.Warn(r.Context(), "failed to delete file record after successful backend delete, retrying",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Error(err))

		// Retry up to 3 times with exponential backoff
		retryDelays := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}
		for i, delay := range retryDelays {
			// Respect context cancellation (client disconnect)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				logger.Info(r.Context(), "retry cancelled due to context cancellation",
					zap.String("user_id", userID),
					zap.String("key", key),
					zap.Int("completed_retries", i))
				w.WriteHeader(http.StatusNoContent)
				return
			}

			if retryErr := h.oss3UC.DeleteFileRecord(r.Context(), userID, key); retryErr == nil {
				logger.Info(r.Context(), "file record deleted successfully after retry",
					zap.String("user_id", userID),
					zap.String("key", key),
					zap.Int("attempt", i+2))
				break
			} else if i == len(retryDelays)-1 {
				// All retries failed — log for manual intervention or async cleanup
				logger.Error(r.Context(), "failed to delete file record after all retries, orphaned record",
					zap.String("user_id", userID),
					zap.String("key", key),
					zap.Error(retryErr))
				RecordOrphanedRecord("delete_failed", userID)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
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

	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Check rate limit
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Get object metadata from backend
	meta, err := h.oss3UC.Backend().HeadObject(r.Context(), bucket, key)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Set response headers
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

// handleCopyObject handles PUT /{bucket}/{key} with X-Amz-Copy-Source header.
func (h *OSS3Handler) handleCopyObject(
	w http.ResponseWriter, r *http.Request,
	dstBucket, dstKey, copySource string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.CopyObject",
		trace.WithAttributes(
			attribute.String("dst_bucket", dstBucket),
			attribute.String("dst_key", dstKey),
			attribute.String("copy_source", copySource),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Check rate limit
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Parse copy source: /{bucket}/{key}
	srcBucket, srcKey, ok := parseS3Path(copySource)
	if !ok {
		WriteS3Error(w, rtcoss3.ErrInvalidCopySource, r.URL.Path, "")
		return
	}

	// Validate source bucket
	if srcBucket != h.bucket {
		WriteS3Error(w, rtcoss3.ErrNoSuchBucket, r.URL.Path, "")
		return
	}

	// Validate source key format and user ownership (defense in depth).
	// The key format includes the user ID (user-{uuid}/{md5}.{ext}), so this
	// check ensures the source key belongs to the requesting user.
	if err := rtcoss3.ValidateKey(srcKey, userID); err != nil {
		WriteS3Error(w, err, r.URL.Path, "")
		return
	}

	// Get source object size for quota check.
	// HeadObject also serves as an existence check — if the source doesn't exist,
	// we fail here before reserving quota or attempting the copy.
	srcMeta, err := h.oss3UC.Backend().HeadObject(r.Context(), srcBucket, srcKey)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Verify source file record exists in DB for this user (TOCTOU protection).
	// This ensures the source is not just present in MinIO but also properly
	// registered to this user in the database. Without this check, a race
	// condition could allow copying a file that was just deleted by its owner.
	srcFile, err := h.oss3UC.GetFileRecord(r.Context(), userID, srcKey)
	if err != nil {
		WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}
	if srcFile == nil {
		// Source file record doesn't exist for this user — access denied.
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Check and reserve quota for the copy destination (prevents quota bypass via copy).
	// Zero-byte copies skip quota reservation — they consume no storage.
	var quotaRequestID string
	if srcMeta.Size > 0 {
		qrid, err := h.oss3UC.CheckAndReserveQuota(r.Context(), userID, srcMeta.Size)
		if err != nil {
			writeQuotaReserveError(w, err, r.URL.Path)
			return
		}
		quotaRequestID = qrid
	}
	// uploadSucceeded tracks whether data reached MinIO.
	// If false at function exit, the defer releases the pending reservation.
	// If true, quota is either committed or will be reconciled later.
	uploadSucceeded := false
	defer func() {
		if !uploadSucceeded && quotaRequestID != "" {
			h.oss3UC.ReleaseQuota(r.Context(), userID, quotaRequestID)
		}
	}()

	// Copy object in backend
	result, err := h.oss3UC.Backend().CopyObject(r.Context(), srcBucket, srcKey, dstBucket, dstKey)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return // uploadSucceeded=false -> defer releases quota
	}
	uploadSucceeded = true

	// Commit quota with retry (idempotent via SETNX marker).
	// Skip for zero-byte copies — no quota was reserved.
	if quotaRequestID != "" {
		if commitErr := h.commitQuotaWithRetry(r.Context(), userID, quotaRequestID, srcMeta.Size); commitErr != nil {
			logger.Error(r.Context(), "quota commit failed after retries; reconciliation will sync",
				zap.String("user_id", userID),
				zap.String("quota_request_id", quotaRequestID),
				zap.Int64("amount", srcMeta.Size),
				zap.Error(commitErr))
			RecordOrphanedQuotaCommit(userID)
		}
	}

	// Create file record for destination with retry logic for idempotency.
	// The underlying file_repo.Create uses upsert (OnConflict), so retries are safe.
	// If all retries fail, log error but return success since data is in MinIO
	// and quota is committed — reconciliation will clean up orphaned objects.
	file := &usecase.FileRecord{
		UserID:      userID,
		Bucket:      dstBucket,
		Key:         dstKey,
		Size:        result.Size,
		ContentType: result.ContentType,
		ETag:        result.ETag,
	}
	if err = h.oss3UC.CreateFileRecord(r.Context(), file); err != nil {
		logger.Warn(r.Context(), "failed to create file record after successful copy, retrying",
			zap.String("user_id", userID),
			zap.String("dst_bucket", dstBucket),
			zap.String("dst_key", dstKey),
			zap.Error(err))

		// Retry up to 3 times with exponential backoff
		retryDelays := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}
		for i, delay := range retryDelays {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				logger.Info(r.Context(), "retry cancelled due to context cancellation",
					zap.String("user_id", userID),
					zap.String("dst_key", dstKey),
					zap.Int("completed_retries", i))
				// Return success since data is in MinIO
				writeCopyObjectResultXML(w, result.ETag, result.LastModified)
				return
			}

			if retryErr := h.oss3UC.CreateFileRecord(r.Context(), file); retryErr == nil {
				logger.Info(r.Context(), "file record created successfully after retry",
					zap.String("user_id", userID),
					zap.String("dst_key", dstKey),
					zap.Int("attempt", i+2))
				break
			} else if i == len(retryDelays)-1 {
				// All retries failed — log for reconciliation
				logger.Error(r.Context(), "failed to create file record after all retries, orphaned object",
					zap.String("user_id", userID),
					zap.String("dst_bucket", dstBucket),
					zap.String("dst_key", dstKey),
					zap.Error(retryErr))
				RecordOrphanedRecord("copy_failed", userID)
			}
		}
	}

	// Return XML response per S3 spec
	writeCopyObjectResultXML(w, result.ETag, result.LastModified)
}

// xmlCopyResult is the XML envelope returned by a successful CopyObject.
type xmlCopyResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// writeCopyObjectResultXML serialises a CopyObjectResult XML response.
// Uses S3TimeFormat for consistency with other S3 responses.
func writeCopyObjectResultXML(w http.ResponseWriter, etag string, lastModified time.Time) {
	resp := xmlCopyResult{
		ETag:         etag,
		LastModified: lastModified.UTC().Format(rtcoss3.S3TimeFormat),
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// handleListObjects handles GET /{bucket} — list objects.
func (h *OSS3Handler) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.ListObjects",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Check rate limit
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Parse query parameters — supports both V1 (marker) and V2 (list-type=2) semantics.
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys := parseMaxKeys(q.Get("max-keys"))

	opts := rtcoss3.ListObjectsOptions{
		Prefix:    prefix,
		Delimiter: delimiter,
		MaxKeys:   maxKeys,
	}

	isV2 := q.Get("list-type") == "2"

	// V1: marker-based pagination
	// V2: continuation-token-based pagination, activated by list-type=2
	if isV2 {
		opts.ContinuationToken = q.Get("continuation-token")
		opts.StartAfter = q.Get("start-after")
	} else {
		opts.Marker = q.Get("marker")
	}

	// List objects from backend
	result, err := h.oss3UC.Backend().ListObjects(r.Context(), bucket, opts)
	if err != nil {
		// Check if this is a continuation token decode error (InvalidArgument)
		if isV2 && opts.ContinuationToken != "" && strings.Contains(err.Error(), "invalid continuation token") {
			WriteS3Error(w, rtcoss3.ErrInvalidArgument, r.URL.Path, "")
			return
		}
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Branch on V1 vs V2 for response serialization
	if isV2 {
		writeListBucketResultV2XML(w, bucket, prefix, q.Get("start-after"), q.Get("continuation-token"), maxKeys, delimiter, result)
	} else {
		writeListBucketResultV1XML(w, bucket, prefix, q.Get("marker"), maxKeys, delimiter, result)
	}
}

// handleDeleteObjects handles POST /{bucket}?delete= — batch delete objects (H4).
// S3 spec: https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html
func (h *OSS3Handler) handleDeleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.DeleteObjects",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context())

	// Check rate limit
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Limit XML request body to 1 MB to prevent memory exhaustion.
	// S3 spec allows up to 1000 keys per request; typical XML is ~50KB.
	const maxDeleteBodySize = 1 << 20 // 1 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxDeleteBodySize)

	// Parse XML request body
	type deleteRequest struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}

	var req deleteRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteS3Error(w, rtcoss3.ErrMalformedXML, r.URL.Path, "")
		return
	}

	// Validate request
	if len(req.Objects) == 0 {
		WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		return
	}
	if len(req.Objects) > 1000 {
		WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		return
	}

	// Validate all keys belong to this user and extract key list
	keys := make([]string, 0, len(req.Objects))
	for _, obj := range req.Objects {
		if err := rtcoss3.ValidateKey(obj.Key, userID); err != nil {
			WriteS3Error(w, err, r.URL.Path, "")
			return
		}
		keys = append(keys, obj.Key)
	}

	// Batch delete from backend
	results, err := h.oss3UC.Backend().DeleteObjects(r.Context(), bucket, keys)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete file records from DB (best-effort, log failures)
	for i, result := range results {
		if result.Code == "" {
			// Successful deletion — delete DB record
			if err := h.oss3UC.DeleteFileRecord(r.Context(), userID, keys[i]); err != nil {
				logger.Warn(r.Context(), "failed to delete file record after successful backend delete",
					zap.String("user_id", userID),
					zap.String("key", keys[i]),
					zap.Error(err))
			}
		}
	}

	// Build XML response
	type xmlDeletedObject struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId,omitempty"`
	}
	type xmlError struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	type deleteResult struct {
		XMLName xml.Name           `xml:"DeleteResult"`
		Deleted []xmlDeletedObject `xml:"Deleted"`
		Errors  []xmlError         `xml:"Error"`
	}

	resp := deleteResult{}
	for i, result := range results {
		if result.Code == "" {
			// Successful deletion
			if !req.Quiet {
				resp.Deleted = append(resp.Deleted, xmlDeletedObject{Key: keys[i]})
			}
		} else {
			// Failed deletion
			resp.Errors = append(resp.Errors, xmlError{
				Key:     keys[i],
				Code:    result.Code,
				Message: result.Message,
			})
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// parseMaxKeys parses the "max-keys" query parameter, clamping to [1, 1000].
// Returns the S3 default of 1000 when the parameter is absent or invalid.
func parseMaxKeys(raw string) int {
	const defaultMaxKeys = 1000
	if raw == "" {
		return defaultMaxKeys
	}
	if mk, err := strconv.Atoi(raw); err == nil && mk > 0 && mk <= 1000 {
		return mk
	}
	return defaultMaxKeys
}

// xmlContent, xmlCommonPrefix, xmlListBucketResultV1, and xmlListBucketResultV2
// are the XML response types for the ListBucketResult S3 response envelope.
// V1 and V2 have different fields per the S3 specification.
type xmlListContent struct {
	XMLName      xml.Name `xml:"Contents"`
	Key          string   `xml:"Key"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
	Size         int64    `xml:"Size"`
	StorageClass string   `xml:"StorageClass"`
}

type xmlListCommonPrefix struct {
	XMLName xml.Name `xml:"CommonPrefixes"`
	Prefix  string   `xml:"Prefix"`
}

// V1 response: uses Marker/NextMarker for pagination
type xmlListBucketResultV1 struct {
	XMLName        xml.Name              `xml:"ListBucketResult"`
	Name           string                `xml:"Name"`
	Prefix         string                `xml:"Prefix"`
	Marker         string                `xml:"Marker"`
	NextMarker     string                `xml:"NextMarker,omitempty"`
	MaxKeys        int                   `xml:"MaxKeys"`
	Delimiter      string                `xml:"Delimiter,omitempty"`
	IsTruncated    bool                  `xml:"IsTruncated"`
	Contents       []xmlListContent      `xml:"Contents"`
	CommonPrefixes []xmlListCommonPrefix `xml:"CommonPrefixes"`
}

// V2 response: uses ContinuationToken/NextContinuationToken and KeyCount
type xmlListBucketResultV2 struct {
	XMLName               xml.Name              `xml:"ListBucketResult"`
	Name                  string                `xml:"Name"`
	Prefix                string                `xml:"Prefix"`
	StartAfter            string                `xml:"StartAfter,omitempty"`
	ContinuationToken     string                `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string                `xml:"NextContinuationToken,omitempty"`
	KeyCount              int                   `xml:"KeyCount"`
	MaxKeys               int                   `xml:"MaxKeys"`
	Delimiter             string                `xml:"Delimiter,omitempty"`
	IsTruncated           bool                  `xml:"IsTruncated"`
	Contents              []xmlListContent      `xml:"Contents"`
	CommonPrefixes        []xmlListCommonPrefix `xml:"CommonPrefixes"`
}

// writeListBucketResultV1XML serialises a V1 ListBucketResult XML response.
func writeListBucketResultV1XML(
	w http.ResponseWriter,
	bucket, prefix, marker string,
	maxKeys int,
	delimiter string,
	result *rtcoss3.ListObjectsResult,
) {
	resp := xmlListBucketResultV1{
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		NextMarker:  result.NextMarker,
		MaxKeys:     maxKeys,
		Delimiter:   delimiter,
		IsTruncated: result.IsTruncated,
	}
	for _, obj := range result.Objects {
		resp.Contents = append(resp.Contents, xmlListContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(rtcoss3.S3TimeFormat),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range result.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, xmlListCommonPrefix{
			Prefix: cp,
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// writeListBucketResultV2XML serialises a V2 ListBucketResult XML response.
func writeListBucketResultV2XML(
	w http.ResponseWriter,
	bucket, prefix, startAfter, continuationToken string,
	maxKeys int,
	delimiter string,
	result *rtcoss3.ListObjectsResult,
) {
	resp := xmlListBucketResultV2{
		Name:                  bucket,
		Prefix:                prefix,
		StartAfter:            startAfter,
		ContinuationToken:     continuationToken,
		NextContinuationToken: result.NextContinuationToken,
		KeyCount:              result.KeyCount,
		MaxKeys:               maxKeys,
		Delimiter:             delimiter,
		IsTruncated:           result.IsTruncated,
	}
	for _, obj := range result.Objects {
		resp.Contents = append(resp.Contents, xmlListContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(rtcoss3.S3TimeFormat),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range result.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, xmlListCommonPrefix{
			Prefix: cp,
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// parseRangeHeader parses HTTP Range header.
// Format: "bytes=start-end"
func parseRangeHeader(rangeHeader string) (start, end int64, hasRange bool, err error) {
	if rangeHeader == "" {
		return 0, 0, false, nil
	}

	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false, fmt.Errorf("invalid range format")
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		return 0, 0, false, fmt.Errorf("invalid range format")
	}

	if parts[0] == "" {
		// Suffix range: "-500" means last 500 bytes
		suffixLen, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false, err
		}
		// Negative offset from end — will be resolved in backend
		return -suffixLen, -1, true, nil
	}

	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false, err
	}

	if parts[1] == "" {
		// Open-ended range: "500-" means from byte 500 to end
		end = -1
	} else {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false, err
		}
	}

	if start < 0 || (end >= 0 && end < start) {
		return 0, 0, false, fmt.Errorf("invalid range values")
	}

	return start, end, true, nil
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
