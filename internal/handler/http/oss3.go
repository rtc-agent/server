package httphandler

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
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
// Format: /{bucket}/{key} or /{bucket}
func parseS3Path(path string) (bucket, key string, ok bool) {
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
func (h *OSS3Handler) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
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

	// TODO: Extract user_id from context (set by SigV4 middleware)
	userID := ExtractUserIDFromContext(r.Context())
	if userID == "" {
		WriteS3Error(w, rtcoss3.ErrAccessDenied, r.URL.Path, "")
		return
	}

	// Check quota
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

	// Upload to backend with compensation
	etag, err := h.oss3UC.Backend().PutObject(r.Context(), bucket, key, r.Body, contentLength, r.Header.Get("Content-Type"))
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Commit quota
	if err = h.oss3UC.CommitQuota(r.Context(), userID, quotaRequestID, contentLength); err != nil {
		// Quota commit failed but upload succeeded — log and continue
		// TODO: integrate with structured logging
		_ = err
	}

	// Create file record in DB with compensation
	if err = h.oss3UC.PutObjectWithComp(r.Context(), userID, bucket, key, contentLength, r.Header.Get("Content-Type"), etag); err != nil {
		WriteS3Error(w, rtcoss3.ErrInternalError, r.URL.Path, "")
		return
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// handleGetObject handles GET /{bucket}/{key} — download object.
func (h *OSS3Handler) handleGetObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// TODO: Extract user_id from context
	userID := ExtractUserIDFromContext(r.Context())
	requestID := ExtractRequestIDFromContext(r.Context()) // Will be extracted from context

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
			// TODO: integrate with structured logging
			_ = closeErr
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
		// TODO: integrate with structured logging
		_ = err
	}
}

// handleDeleteObject handles DELETE /{bucket}/{key} — delete object.
func (h *OSS3Handler) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// TODO: Extract user_id from context
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
		// TODO: integrate with structured logging
		_ = err
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleHeadObject handles HEAD /{bucket}/{key} — get object metadata.
func (h *OSS3Handler) handleHeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// TODO: Extract user_id from context
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
func (h *OSS3Handler) handleCopyObject(w http.ResponseWriter, r *http.Request, dstBucket, dstKey, copySource string) {
	// TODO: Extract user_id from context
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
		// TODO: integrate with structured logging
		_ = err
	}

	// Return XML response per S3 spec
	type copyResult struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}
	resp := copyResult{
		ETag:         result.ETag,
		LastModified: result.LastModified.UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(resp)
}

// handleListObjects handles GET /{bucket} — list objects.
func (h *OSS3Handler) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	// TODO: Extract user_id from context
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
	maxKeysStr := r.URL.Query().Get("max-keys")
	marker := r.URL.Query().Get("marker")

	maxKeys := 1000 // default
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
			maxKeys = mk
		}
	}

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

	// Build XML response per S3 spec
	type xmlContent struct {
		XMLName      xml.Name `xml:"Contents"`
		Key          string   `xml:"Key"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
		Size         int64    `xml:"Size"`
		StorageClass string   `xml:"StorageClass"`
	}
	type xmlCommonPrefix struct {
		XMLName xml.Name `xml:"CommonPrefixes"`
		Prefix  string   `xml:"Prefix"`
	}
	type xmlListBucketResult struct {
		XMLName        xml.Name          `xml:"ListBucketResult"`
		Name           string            `xml:"Name"`
		Prefix         string            `xml:"Prefix"`
		Marker         string            `xml:"Marker"`
		MaxKeys        int               `xml:"MaxKeys"`
		IsTruncated    bool              `xml:"IsTruncated"`
		Contents       []xmlContent      `xml:"Contents"`
		CommonPrefixes []xmlCommonPrefix `xml:"CommonPrefixes"`
	}

	resp := xmlListBucketResult{
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		MaxKeys:     maxKeys,
		IsTruncated: result.IsTruncated,
	}

	for _, obj := range result.Objects {
		resp.Contents = append(resp.Contents, xmlContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(time.RFC3339),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}

	for _, cp := range result.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, xmlCommonPrefix{
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

// isQuotaExceededError checks if error is quota exceeded.
func isQuotaExceededError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "quota exceeded")
}

// mapBackendError maps backend errors to S3 errors.
func mapBackendError(err error) *rtcoss3.S3Error {
	if err == nil {
		return nil
	}
	errStr := err.Error()
	if strings.Contains(errStr, "not found") || strings.Contains(errStr, "NoSuchKey") {
		return rtcoss3.ErrKeyNotFound
	}
	if strings.Contains(errStr, "access denied") || strings.Contains(errStr, "AccessDenied") {
		return rtcoss3.ErrAccessDenied
	}
	return rtcoss3.ErrInternalError
}
