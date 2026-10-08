package httphandler

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleCompleteMultipartUpload handles POST /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleCompleteMultipartUpload(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(
		r.Context(), "oss3.complete_multipart_upload",
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

	// Limit XML request body to 1 MB to prevent memory exhaustion.
	// A typical complete request with 10 000 parts is ~1 MB of XML;
	// anything larger is either malformed or malicious.
	r.Body = http.MaxBytesReader(w, r.Body, maxXMLRequestBodySize)

	// Parse XML request body
	type completeRequest struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}

	var req completeRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		// Check if the error is due to request body being too large
		if strings.Contains(err.Error(), "http: request body too large") {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrEntityTooLarge, r.URL.Path, "")
			return
		}
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMalformedXML, r.URL.Path, "")
		return
	}

	// Convert to backend format
	parts := make([]rtcoss3.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = rtcoss3.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		}
	}

	// Instant upload check: Since the key contains MD5, if the file already exists
	// in both DB and MinIO, the upload has already completed successfully.
	// For multipart uploads, return success directly and clean up the multipart record.
	existingFile, err := h.oss3UC.GetFileRecord(ctx, userID, key)
	if err != nil {
		// Database error — fail fast
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	if existingFile != nil {
		// DB record exists, verify MinIO file still exists
		existingMeta, headErr := h.oss3UC.Backend().HeadObject(ctx, bucket, key)
		if headErr == nil && existingMeta.Key != "" {
			// Both DB and MinIO confirm file exists (instant upload hit)
			span.SetAttributes(attribute.String("oss.upload_source", "multipart"))
			logger.Info(ctx, "instant upload: multipart hit",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.String("upload_id", uploadID))
			RecordInstantUpload("multipart")

			// OSS3-27 fix: Compensation logic moved to usecase layer.
			// Release quota committed during UploadPart (parts are discarded, not committed to a file)
			h.oss3UC.ReleaseMultipartUploadQuota(ctx, userID, uploadID)

			// Abort the multipart upload to clean up
			if abortErr := h.oss3UC.Backend().AbortMultipartUpload(ctx, bucket, key, uploadID); abortErr != nil {
				span.RecordError(abortErr)
				logger.Warn(ctx, "instant upload: failed to abort multipart upload",
					zap.String("user_id", userID),
					zap.String("upload_id", uploadID),
					zap.Error(abortErr))
			}
			// Delete multipart upload record from DB
			if deleteErr := h.oss3UC.DeleteMultipartUploadRecord(ctx, uploadID); deleteErr != nil {
				span.RecordError(deleteErr)
				logger.Warn(ctx, "instant upload: failed to delete multipart upload record",
					zap.String("user_id", userID),
					zap.String("upload_id", uploadID),
					zap.Error(deleteErr))
			}

			// Return success with existing file's ETag
			writeCompleteMultipartResult(w, bucket, key, existingMeta.ETag)
			return
		}
		// DB has record but MinIO file missing — consistency violation, fall through
		span.SetAttributes(attribute.Bool("db.consistency_violation", true))
		RecordConsistencyViolation("db_has_minio_missing")
		logger.Warn(ctx, "instant upload: DB record exists but MinIO file missing, completing multipart upload",
			zap.String("user_id", userID),
			zap.String("key", key))
	}

	// Complete multipart upload with compensation
	etag, err := h.oss3UC.CompleteMultipartUploadWithComp(
		r.Context(), userID, bucket, key, uploadID, parts)
	if err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Return XML response per S3 spec
	writeCompleteMultipartResult(w, bucket, key, etag)

	RecordMultipartUploadEnd()
}

// writeCompleteMultipartResult writes the CompleteMultipartUpload XML response.
// Shared by the normal completion path and the instant upload path.
func writeCompleteMultipartResult(w http.ResponseWriter, bucket, key, etag string) {
	type completeResult struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}
	resp := completeResult{
		Location: fmt.Sprintf("/%s/%s", bucket, key),
		Bucket:   bucket,
		Key:      key,
		ETag:     etag,
	}
	writeXMLResponse(w, http.StatusOK, resp)
}
