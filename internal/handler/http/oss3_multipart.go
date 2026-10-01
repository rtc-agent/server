package httphandler

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"

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
	oss3UC *usecase.OSS3Usecase
	bucket string
}

// NewOSS3MultipartHandler creates a new multipart upload handler.
func NewOSS3MultipartHandler(oss3UC *usecase.OSS3Usecase, bucket string) *OSS3MultipartHandler {
	return &OSS3MultipartHandler{
		oss3UC: oss3UC,
		bucket: bucket,
	}
}

// handleCreateMultipartUpload handles POST /{bucket}/{key}?uploads
func (h *OSS3MultipartHandler) handleCreateMultipartUpload(
	w http.ResponseWriter, r *http.Request, bucket, key string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(
		r.Context(), "oss3.CreateMultipartUpload",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Check rate limit
	requestID := ExtractRequestIDFromContext(r.Context())
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Create multipart upload in backend
	result, err := h.oss3UC.Backend().CreateMultipartUpload(r.Context(), bucket, key, contentType)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Create upload record in DB for tracking
	err = h.oss3UC.CreateMultipartUploadRecord(
		r.Context(), userID, bucket, key, result.UploadID)
	if err != nil {
		// DB failed but backend upload created — orphaned upload
		logger.Warn(r.Context(), "failed to create multipart upload record",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.String("upload_id", result.UploadID),
			zap.Error(err))
	}

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

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// handleUploadPart handles PUT /{bucket}/{key}?partNumber={n}&uploadId={id}
func (h *OSS3MultipartHandler) handleUploadPart(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.UploadPart",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("upload_id", uploadID),
			attribute.String("user_id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Check rate limit before proceeding
	if err := h.oss3UC.CheckRateLimitOrReject(r.Context(), userID, ExtractRequestIDFromContext(r.Context())); err != nil {
		WriteS3Error(w, rtcoss3.ErrSlowDown, r.URL.Path, "")
		return
	}

	// Parse part number
	partNumberStr := r.URL.Query().Get("partNumber")
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		WriteS3Error(w, rtcoss3.ErrInvalidPartNumber, r.URL.Path, "")
		return
	}

	contentLength := r.ContentLength
	if contentLength < 0 {
		WriteS3Error(w, rtcoss3.ErrMissingContentLength, r.URL.Path, "")
		return
	}

	// Check quota for this part
	quotaRequestID, err := h.oss3UC.CheckAndReserveQuota(r.Context(), userID, contentLength)
	if err != nil {
		if isQuotaExceededError(err) {
			WriteS3Error(w, rtcoss3.ErrRequestQuotaExceeded, r.URL.Path, "")
		} else {
			WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		}
		return
	}

	// Ensure quota is released on failure
	defer func() {
		if err != nil {
			h.oss3UC.ReleaseQuota(r.Context(), userID, quotaRequestID)
		}
	}()

	// Upload part to backend
	etag, err := h.oss3UC.Backend().UploadPart(
		r.Context(), bucket, key, uploadID, partNumber, r.Body, contentLength)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Commit quota
	if err = h.oss3UC.CommitQuota(r.Context(), userID, quotaRequestID, contentLength); err != nil {
		// Quota commit failed but upload succeeded
		logger.Warn(r.Context(), "failed to commit quota after successful part upload",
			zap.String("user_id", userID),
			zap.String("upload_id", uploadID),
			zap.Int("part_number", partNumber),
			zap.Error(err))
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// handleCompleteMultipartUpload handles POST /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleCompleteMultipartUpload(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(
		r.Context(), "oss3.CompleteMultipartUpload",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("upload_id", uploadID),
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
		WriteS3Error(w, rtcoss3.ErrMalformedXML, r.URL.Path, "")
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
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	if existingFile != nil {
		// DB record exists, verify MinIO file still exists
		existingMeta, headErr := h.oss3UC.Backend().HeadObject(ctx, bucket, key)
		if headErr == nil && existingMeta.Key != "" {
			// Both DB and MinIO confirm file exists (instant upload hit)
			span.SetAttributes(attribute.String("instant_upload", "multipart"))
			logger.Info(ctx, "instant upload: multipart hit",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.String("upload_id", uploadID))
			RecordInstantUpload("multipart")

			// Release quota committed during UploadPart (parts are discarded, not committed to a file)
			partsTotalSize, sizeErr := h.oss3UC.GetMultipartUploadTotalSize(ctx, uploadID)
			if sizeErr != nil {
				logger.Warn(ctx, "instant upload: failed to get parts total size for quota release",
					zap.String("user_id", userID),
					zap.String("upload_id", uploadID),
					zap.Error(sizeErr))
			} else if partsTotalSize > 0 {
				if adjustErr := h.oss3UC.AdjustQuota(ctx, userID, -partsTotalSize); adjustErr != nil {
					logger.Warn(ctx, "instant upload: failed to release quota",
						zap.String("user_id", userID),
						zap.Int64("parts_total_size", partsTotalSize),
						zap.Error(adjustErr))
				}
			}

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
		span.SetAttributes(attribute.Bool("db_consistency_violation", true))
		RecordConsistencyViolation("db_has_minio_missing")
		logger.Warn(ctx, "instant upload: DB record exists but MinIO file missing, completing multipart upload",
			zap.String("user_id", userID),
			zap.String("key", key))
	}

	// Complete multipart upload with compensation
	etag, err := h.oss3UC.CompleteMultipartUploadWithComp(
		r.Context(), userID, bucket, key, uploadID, parts)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Return XML response per S3 spec
	writeCompleteMultipartResult(w, bucket, key, etag)
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
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// handleAbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleAbortMultipartUpload(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(
		r.Context(), "oss3.AbortMultipartUpload",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("upload_id", uploadID),
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

	// Abort multipart upload in backend
	if err := h.oss3UC.Backend().AbortMultipartUpload(r.Context(), bucket, key, uploadID); err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete multipart upload record from DB
	if err := h.oss3UC.DeleteMultipartUploadRecord(r.Context(), uploadID); err != nil {
		logger.Warn(r.Context(), "failed to delete multipart upload record after abort",
			zap.String("upload_id", uploadID),
			zap.Error(err))
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListParts handles GET /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleListParts(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.ListParts",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("upload_id", uploadID),
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

	// List parts from backend
	parts, err := h.oss3UC.Backend().ListParts(r.Context(), bucket, key, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Build XML response per S3 spec
	type xmlPart struct {
		XMLName      xml.Name `xml:"Part"`
		PartNumber   int      `xml:"PartNumber"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
		Size         int64    `xml:"Size"`
	}
	type xmlListPartsResult struct {
		XMLName              xml.Name  `xml:"ListPartsResult"`
		Bucket               string    `xml:"Bucket"`
		Key                  string    `xml:"Key"`
		UploadID             string    `xml:"UploadId"`
		Parts                []xmlPart `xml:"Part"`
		IsTruncated          bool      `xml:"IsTruncated"`
		MaxParts             int       `xml:"MaxParts"`
		NextPartNumberMarker int       `xml:"NextPartNumberMarker"`
	}

	resp := xmlListPartsResult{
		Bucket:      bucket,
		Key:         key,
		UploadID:    uploadID,
		MaxParts:    1000,
		IsTruncated: false,
	}

	for _, p := range parts {
		resp.Parts = append(resp.Parts, xmlPart{
			PartNumber:   p.PartNumber,
			LastModified: p.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         p.ETag,
			Size:         p.Size,
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// ServeHTTP routes multipart upload requests to the appropriate handler.
func (h *OSS3MultipartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract bucket and key from path
	bucket, key, ok := parseS3Path(r.URL.Path)
	if !ok {
		WriteS3Error(w, rtcoss3.ErrInvalidURI, r.URL.Path, "")
		return
	}

	// Validate bucket name
	if bucket != h.bucket {
		WriteS3Error(w, rtcoss3.ErrNoSuchBucket, r.URL.Path, "")
		return
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
			WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	case http.MethodPut:
		if uploadID != "" {
			h.handleUploadPart(w, r, bucket, key, uploadID)
		} else {
			WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	case http.MethodDelete:
		if uploadID != "" {
			h.handleAbortMultipartUpload(w, r, bucket, key, uploadID)
		} else {
			WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	case http.MethodGet:
		if uploadID != "" {
			h.handleListParts(w, r, bucket, key, uploadID)
		} else {
			WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		}
	default:
		WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
	}
}
