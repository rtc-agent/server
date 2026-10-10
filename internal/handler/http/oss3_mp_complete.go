package httphandler

import (
	"context"
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

// completePart represents a part in the complete multipart upload request.
type completePart struct {
	PartNumber int
	ETag       string
}

// parseCompleteUploadRequest parses and validates the complete multipart upload XML request.
// Returns the parsed parts or writes an error response and returns nil.
func (h *OSS3MultipartHandler) parseCompleteUploadRequest(
	w http.ResponseWriter, r *http.Request,
) []completePart {
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
			return nil
		}
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMalformedXML, r.URL.Path, "")
		return nil
	}

	// Validate parts count: S3 spec limits to 10000 parts maximum
	if len(req.Parts) > 10000 {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMalformedXML, r.URL.Path, "")
		return nil
	}

	// Validate part numbers: must be in range [1, 10000] and unique
	seen := make(map[int]bool, len(req.Parts))
	for _, p := range req.Parts {
		if p.PartNumber < 1 || p.PartNumber > 10000 {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidPartNumber, r.URL.Path, "")
			return nil
		}
		if seen[p.PartNumber] {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidPart, r.URL.Path, "")
			return nil
		}
		seen[p.PartNumber] = true
	}

	// Convert to backend format
	parts := make([]completePart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = completePart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		}
	}
	return parts
}

// cleanupInvalidInstantUpload cleans up resources when an instant upload is rejected
// due to content-type or size violation.
func (h *OSS3MultipartHandler) cleanupInvalidInstantUpload(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID, userID string,
	reason string, s3err *rtcoss3.S3Error,
) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("oss.upload_source", "multipart_repair_rejected"))
	span.SetStatus(codes.Error, reason)

	// Delete the invalid file from MinIO
	if delErr := h.oss3UC.Backend().DeleteObject(ctx, bucket, key); delErr != nil {
		logger.Error(ctx, "instant upload: failed to delete invalid MinIO file",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Error(delErr))
	}

	// Clean up the multipart upload
	h.oss3UC.ReleaseMultipartUploadQuota(ctx, userID, uploadID)
	if abortErr := h.oss3UC.Backend().AbortMultipartUpload(ctx, bucket, key, uploadID); abortErr != nil {
		logger.Warn(ctx, "instant upload: failed to abort multipart upload",
			zap.String("user_id", userID),
			zap.String("upload_id", uploadID),
			zap.Error(abortErr))
	}
	if deleteErr := h.oss3UC.DeleteMultipartUploadRecord(ctx, uploadID); deleteErr != nil {
		logger.Warn(ctx, "instant upload: failed to delete multipart upload record",
			zap.String("user_id", userID),
			zap.String("upload_id", uploadID),
			zap.Error(deleteErr))
	}

	rtcoss3.WriteS3Error(w, s3err, r.URL.Path, "")
}

// checkInstantUploadValidity checks if an existing file is valid for instant upload.
// Returns the file metadata if valid, or handles the error response and returns nil.
func (h *OSS3MultipartHandler) checkInstantUploadValidity(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID, userID string,
) *rtcoss3.ObjectMeta {
	span := trace.SpanFromContext(ctx)

	existingFile, err := h.oss3UC.GetFileRecord(ctx, userID, key)
	if err != nil {
		// Database error — fail fast
		span.SetStatus(codes.Error, "internal_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return nil
	}

	if existingFile == nil {
		// No existing file, proceed with normal upload
		return nil
	}

	// DB record exists, verify MinIO file still exists
	existingMeta, headErr := h.oss3UC.Backend().HeadObject(ctx, bucket, key)
	if headErr != nil || existingMeta.Key == "" {
		// DB has record but MinIO file missing — consistency violation, fall through
		span.SetAttributes(attribute.Bool("db.consistency_violation", true))
		RecordConsistencyViolation("db_has_minio_missing")
		logger.Warn(ctx, "instant upload: DB record exists but MinIO file missing, completing multipart upload",
			zap.String("user_id", userID),
			zap.String("key", key))
		return nil
	}

	// P1-6 fix: Validate content-type to prevent presigned URL bypass attacks
	if !rtcoss3.IsAllowedContentType(existingMeta.ContentType) {
		logger.Warn(ctx, "instant upload: MinIO file has disallowed content-type, deleting",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.String("content_type", existingMeta.ContentType))
		h.cleanupInvalidInstantUpload(ctx, w, r, bucket, key, uploadID, userID,
			"content_type_violation", rtcoss3.ErrUnsupportedContentType)
		return nil
	}

	// Validate file size
	if h.maxFileSizeBytes > 0 && existingMeta.Size > h.maxFileSizeBytes {
		logger.Warn(ctx, "instant upload: MinIO file exceeds max size, deleting",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Int64("size", existingMeta.Size),
			zap.Int64("max_size", h.maxFileSizeBytes))
		h.cleanupInvalidInstantUpload(ctx, w, r, bucket, key, uploadID, userID,
			"size_violation", rtcoss3.ErrEntityTooLarge)
		return nil
	}

	return &existingMeta
}

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

	// Parse and validate the complete upload request
	reqParts := h.parseCompleteUploadRequest(w, r)
	if reqParts == nil {
		return
	}

	// Convert to backend format
	parts := make([]rtcoss3.CompletedPart, len(reqParts))
	for i, p := range reqParts {
		parts[i] = rtcoss3.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		}
	}

	// Validate minimum part size (S3 spec: all parts except the last must be >= 5MB).
	// We query the backend for actual part sizes since the Complete request only
	// contains part numbers and ETags.
	if len(parts) > 1 {
		uploadedParts, listErr := h.oss3UC.Backend().ListParts(ctx, bucket, key, uploadID)
		if listErr != nil {
			span.SetStatus(codes.Error, "list_parts_error")
			span.RecordError(listErr)
			rtcoss3.WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
			return
		}
		// Build a map of part number -> size for quick lookup
		partSizes := make(map[int]int64, len(uploadedParts))
		for _, up := range uploadedParts {
			partSizes[up.PartNumber] = up.Size
		}
		// Determine the last part number (the largest part number in the request)
		maxPartNum := 0
		for _, p := range parts {
			if p.PartNumber > maxPartNum {
				maxPartNum = p.PartNumber
			}
		}
		// Validate all parts except the last are >= MinPartSize
		for _, p := range parts {
			if p.PartNumber == maxPartNum {
				continue // last part has no minimum size requirement
			}
			if size, ok := partSizes[p.PartNumber]; ok && size < rtcoss3.MinPartSize {
				logger.Warn(ctx, "complete multipart upload: part smaller than minimum 5MB",
					zap.String("user_id", userID),
					zap.String("key", key),
					zap.String("upload_id", uploadID),
					zap.Int("part_number", p.PartNumber),
					zap.Int64("size", size),
					zap.Int64("min_size", rtcoss3.MinPartSize),
				)
				rtcoss3.WriteS3Error(w, rtcoss3.ErrEntityTooSmall, r.URL.Path, "")
				return
			}
		}
	}

	// Instant upload check: Since the key contains MD5, if the file already exists
	// in both DB and MinIO, the upload has already completed successfully.
	if existingMeta := h.checkInstantUploadValidity(ctx, w, r, bucket, key, uploadID, userID); existingMeta != nil {
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
