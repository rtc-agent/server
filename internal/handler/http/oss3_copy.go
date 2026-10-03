package httphandler

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleCopyObject handles PUT /{bucket}/{key} with X-Amz-Copy-Source header.
func (h *OSS3Handler) handleCopyObject(
	w http.ResponseWriter, r *http.Request,
	dstBucket, dstKey, copySource string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.copy_object",
		trace.WithAttributes(
			attribute.String("oss.destination.bucket", dstBucket),
			attribute.String("oss.destination.key", dstKey),
			attribute.String("oss.copy_source", copySource),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Parse copy source: /{bucket}/{key}
	// Bug #6 fix: URL-decode the copy source header value.
	// net/http does not auto-decode header values (only URL paths).
	decodedSource, err := url.PathUnescape(copySource)
	if err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidCopySource, r.URL.Path, "")
		return
	}
	srcBucket, srcKey, ok := parseS3Path(decodedSource)
	if !ok {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidCopySource, r.URL.Path, "")
		return
	}

	// Validate source bucket
	if srcBucket != h.bucket {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrBucketNotFound, r.URL.Path, "")
		return
	}

	// Validate source key format and user ownership (defense in depth).
	// The key format includes the user ID (user-{uuid}/{md5}.{ext}), so this
	// check ensures the source key belongs to the requesting user.
	if err := rtcoss3.ValidateKey(srcKey, userID); err != nil {
		rtcoss3.WriteS3Error(w, err, r.URL.Path, "")
		return
	}

	// Get source object size for quota check.
	// HeadObject also serves as an existence check — if the source doesn't exist,
	// we fail here before reserving quota or attempting the copy.
	srcMeta, err := h.oss3UC.Backend().HeadObject(r.Context(), srcBucket, srcKey)
	if err != nil {
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Verify source file record exists in DB for this user (TOCTOU protection).
	// This ensures the source is not just present in MinIO but also properly
	// registered to this user in the database. Without this check, a race
	// condition could allow copying a file that was just deleted by its owner.
	srcFile, err := h.oss3UC.GetFileRecord(r.Context(), userID, srcKey)
	if err != nil {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}
	if srcFile == nil {
		// Source file record doesn't exist for this user — access denied.
		rtcoss3.WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
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
	// Bug #15 fix: Use Background context to avoid cancellation issues.
	defer func() {
		if !uploadSucceeded && quotaRequestID != "" {
			h.oss3UC.ReleaseQuota(context.Background(), userID, quotaRequestID)
		}
	}()

	// Copy object in backend
	result, err := h.oss3UC.Backend().CopyObject(r.Context(), srcBucket, srcKey, dstBucket, dstKey)
	if err != nil {
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return // uploadSucceeded=false -> defer releases quota
	}
	uploadSucceeded = true

	// Commit quota with retry (idempotent via SETNX marker).
	// Skip for zero-byte copies — no quota was reserved.
	if quotaRequestID != "" {
		if commitErr := commitQuotaWithRetry(r.Context(), h.oss3UC, userID, quotaRequestID, srcMeta.Size); commitErr != nil {
			logger.Error(r.Context(), "quota commit failed after retries; reconciliation will sync",
				zap.String("user_id", userID),
				zap.String("quota_request_id", quotaRequestID),
				zap.Int64("amount", srcMeta.Size),
				zap.Error(commitErr))
			RecordOrphanedQuotaCommit()
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

		bo := backoff.NewExponentialBackOff()
		bo.InitialInterval = 100 * time.Millisecond
		bo.MaxInterval = 2 * time.Second

		_, retryErr := backoff.Retry(r.Context(), func() (struct{}, error) {
			return struct{}{}, h.oss3UC.CreateFileRecord(r.Context(), file)
		},
			backoff.WithBackOff(bo),
			backoff.WithMaxTries(3),
		)
		if retryErr != nil {
			// All retries failed — log for reconciliation
			logger.Error(r.Context(), "failed to create file record after all retries, orphaned object",
				zap.String("user_id", userID),
				zap.String("dst_bucket", dstBucket),
				zap.String("dst_key", dstKey),
				zap.Error(retryErr))
			RecordOrphanedRecord("copy_failed")
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
	writeXMLResponse(w, http.StatusOK, resp)
}
