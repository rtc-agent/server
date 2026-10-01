package httphandler

import (
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
	// Extract bucket and key from path: /{bucket}/{key}
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
	default:
		WriteS3Error(w, rtcoss3.ErrMethodNotAllowed, r.URL.Path, "")
	}
}

// isMultipartRequest checks if the request is a multipart upload operation.
func isMultipartRequest(r *http.Request) bool {
	q := r.URL.Query()
	return q.Has("uploads") || q.Has("uploadId")
}

// parseS3Path extracts bucket and key from S3 path-style URL.
// Format: /s3/{bucket}/{key} or /s3/{bucket}
func parseS3Path(path string) (bucket, key string, ok bool) {
	// Remove /s3 prefix
	path = strings.TrimPrefix(path, "/s3")
	// Remove leading slash
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", "", false
	}

	parts := strings.SplitN(path, "/", 2)
	bucket = parts[0]
	if len(parts) > 1 {
		key = parts[1]
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

	userID := ExtractUserIDFromContext(r.Context())
	if userID == "" {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Instant upload check: Since the key contains MD5 (format: user-{userID}/{md5-hash}.{ext}),
	// the same key implies the same content. Check DB and MinIO consistency:
	// 1. Both DB and MinIO have the file -> instant upload hit, return success
	// 2. DB has record but MinIO missing -> consistency violation, log warning and re-upload
	// 3. MinIO has file but DB missing -> repair DB record, return success
	// 4. Neither has file -> normal upload flow
	//
	// Note: Concurrent uploads of the same key are safe due to content-addressing.
	// The upsert in Create() ensures eventual consistency. Race conditions only waste
	// bandwidth but don't corrupt data.
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
		meta, headErr := h.oss3UC.Backend().HeadObject(ctx, bucket, key)
		if headErr == nil && meta.Key != "" {
			// Both DB and MinIO confirm file exists (instant upload hit)
			span.SetAttributes(attribute.String("instant_upload", "db_hit"))
			logger.Info(ctx, "instant upload: DB and MinIO hit",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.Int64("size", existingFile.Size))
			RecordInstantUpload("db_hit")
			w.Header().Set("ETag", existingFile.ETag)
			w.WriteHeader(http.StatusOK)
			return
		}
		// DB has record but MinIO file missing — consistency violation, fall through to normal upload
		span.SetAttributes(attribute.Bool("db_consistency_violation", true))
		RecordConsistencyViolation("db_has_minio_missing")
		logger.Warn(ctx, "instant upload: DB record exists but MinIO file missing, re-uploading",
			zap.String("user_id", userID),
			zap.String("key", key))
	}

	// DB has no record (or consistency violation), check if MinIO has the file (consistency repair)
	existingMeta, err := h.oss3UC.Backend().HeadObject(ctx, bucket, key)
	if err == nil && existingMeta.Key != "" {
		// MinIO has file but DB missing — repair DB record (instant upload + consistency repair)
		span.SetAttributes(attribute.String("instant_upload", "minio_repair"))
		RecordConsistencyViolation("minio_has_db_missing")
		logger.Info(ctx, "instant upload: MinIO hit, repairing DB record",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Int64("size", existingMeta.Size))
		RecordInstantUpload("minio_repair")
		if repairErr := h.oss3UC.CreateFileRecord(ctx, &usecase.FileRecord{
			UserID:      userID,
			Bucket:      bucket,
			Key:         key,
			Size:        existingMeta.Size,
			ContentType: existingMeta.ContentType,
			ETag:        existingMeta.ETag,
		}); repairErr != nil {
			// Repair failed, log but continue (MinIO file exists)
			span.RecordError(repairErr)
			RecordInstantUploadRepairError()
			logger.Warn(ctx, "instant upload: DB repair failed",
				zap.String("user_id", userID),
				zap.String("key", key),
				zap.Error(repairErr))
		}
		w.Header().Set("ETag", existingMeta.ETag)
		w.WriteHeader(http.StatusOK)
		return
	}
	// MinIO also has no file, continue with normal upload flow

	// Reserve quota; release on any subsequent failure.
	quotaRequestID, err := h.oss3UC.CheckAndReserveQuota(r.Context(), userID, contentLength)
	if err != nil {
		writeQuotaReserveError(w, err, r.URL.Path)
		return
	}
	defer func() {
		if err != nil {
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
		return
	}

	// Commit quota
	if err = h.oss3UC.CommitQuota(r.Context(), userID, quotaRequestID, contentLength); err != nil {
		// Quota commit failed but upload succeeded — log and continue
		logger.Warn(r.Context(), "failed to commit quota after successful upload",
			zap.String("user_id", userID),
			zap.String("quota_request_id", quotaRequestID),
			zap.Error(err))
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
		if closeErr := obj.Close(); closeErr != nil {
			logger.Warn(r.Context(), "failed to close object body",
				zap.String("bucket", bucket),
				zap.String("key", key),
				zap.Error(closeErr))
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

	// Stream object body
	if _, err := io.Copy(w, obj); err != nil {
		logger.Warn(r.Context(), "failed to stream object body to client",
			zap.String("bucket", bucket),
			zap.String("key", key),
			zap.Error(err))
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

	// Delete file record from DB
	if err := h.oss3UC.DeleteFileRecord(r.Context(), userID, key); err != nil {
		// DB failed but backend delete succeeded — orphaned record
		logger.Warn(r.Context(), "failed to delete file record after successful backend delete",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Error(err))
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

	// Copy object in backend
	result, err := h.oss3UC.Backend().CopyObject(r.Context(), srcBucket, srcKey, dstBucket, dstKey)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Create file record for destination
	file := &usecase.FileRecord{
		UserID:      userID,
		Bucket:      dstBucket,
		Key:         dstKey,
		Size:        result.Size,
		ContentType: result.ContentType,
		ETag:        result.ETag,
	}
	if err = h.oss3UC.CreateFileRecord(r.Context(), file); err != nil {
		// DB failed but copy succeeded — orphaned object
		logger.Warn(r.Context(), "failed to create file record after successful copy",
			zap.String("user_id", userID),
			zap.String("dst_bucket", dstBucket),
			zap.String("dst_key", dstKey),
			zap.Error(err))
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
func writeCopyObjectResultXML(w http.ResponseWriter, etag string, lastModified time.Time) {
	resp := xmlCopyResult{
		ETag:         etag,
		LastModified: lastModified.UTC().Format(time.RFC3339),
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

	// Parse query parameters
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	marker := r.URL.Query().Get("marker")
	maxKeys := parseMaxKeys(r.URL.Query().Get("max-keys"))

	opts := rtcoss3.ListObjectsOptions{
		Prefix:    prefix,
		Delimiter: delimiter,
		Marker:    marker,
		MaxKeys:   maxKeys,
	}

	// List objects from backend
	result, err := h.oss3UC.Backend().ListObjects(r.Context(), bucket, opts)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	writeListBucketResultXML(w, bucket, prefix, marker, maxKeys, result)
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

// xmlContent, xmlCommonPrefix, and xmlListBucketResult are the XML response
// types for the ListBucketResult S3 response envelope.
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

type xmlListBucketResult struct {
	XMLName        xml.Name              `xml:"ListBucketResult"`
	Name           string                `xml:"Name"`
	Prefix         string                `xml:"Prefix"`
	Marker         string                `xml:"Marker"`
	MaxKeys        int                   `xml:"MaxKeys"`
	IsTruncated    bool                  `xml:"IsTruncated"`
	Contents       []xmlListContent      `xml:"Contents"`
	CommonPrefixes []xmlListCommonPrefix `xml:"CommonPrefixes"`
}

// writeListBucketResultXML serialises a ListBucketResult XML response.
func writeListBucketResultXML(
	w http.ResponseWriter,
	bucket, prefix, marker string,
	maxKeys int,
	result *rtcoss3.ListObjectsResult,
) {
	resp := xmlListBucketResult{
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		MaxKeys:     maxKeys,
		IsTruncated: result.IsTruncated,
	}
	for _, obj := range result.Objects {
		resp.Contents = append(resp.Contents, xmlListContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(time.RFC3339),
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
	case errors.Is(err, rtcoss3.ErrInsufficientStorage):
		return rtcoss3.ErrInsufficientStorage
	default:
		return rtcoss3.ErrInternalError
	}
}
