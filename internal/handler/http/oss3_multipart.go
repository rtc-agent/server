package httphandler

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
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
func (h *OSS3MultipartHandler) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// TODO: Extract user_id from context (set by SigV4 middleware)
	userID := "user-placeholder"

	// Check rate limit
	requestID := "req-placeholder"
	if _, err := h.oss3UC.CheckRateLimit(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrRateLimitExceeded, r.URL.Path, "")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Create multipart upload in backend
	result, err := h.oss3UC.Backend().CreateMultipartUpload(r.Context(), bucket, key, contentType)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Create upload record in DB for tracking
	if err = h.oss3UC.CreateMultipartUploadRecord(r.Context(), userID, bucket, key, result.UploadID); err != nil {
		// DB failed but backend upload created — orphaned upload
		// TODO: integrate with structured logging
		_ = err
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
func (h *OSS3MultipartHandler) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	// TODO: Extract user_id from context
	userID := "user-placeholder"

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
	etag, err := h.oss3UC.Backend().UploadPart(r.Context(), bucket, key, uploadID, partNumber, r.Body, contentLength)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Commit quota
	if err = h.oss3UC.CommitQuota(r.Context(), userID, quotaRequestID, contentLength); err != nil {
		// Quota commit failed but upload succeeded
		// TODO: integrate with structured logging
		_ = err
	}

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// handleCompleteMultipartUpload handles POST /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	// TODO: Extract user_id from context
	userID := "user-placeholder"
	requestID := "req-placeholder"

	// Check rate limit
	if _, err := h.oss3UC.CheckRateLimit(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrRateLimitExceeded, r.URL.Path, "")
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

	// Complete multipart upload in backend
	etag, err := h.oss3UC.Backend().CompleteMultipartUpload(r.Context(), bucket, key, uploadID, parts)
	if err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Get total size from backend (need to query)
	// TODO: Backend should return size in CompleteMultipartUpload result
	meta, err := h.oss3UC.Backend().HeadObject(r.Context(), bucket, key)
	if err != nil {
		// Upload succeeded but we can't get metadata
		// TODO: integrate with structured logging
		_ = err
	}

	// Create file record in DB
	file := &usecase.FileRecord{
		UserID:      userID,
		Bucket:      bucket,
		Key:         key,
		Size:        meta.Size,
		ContentType: meta.ContentType,
		ETag:        etag,
	}
	if err = h.oss3UC.CreateFileRecord(r.Context(), file); err != nil {
		// DB failed but upload succeeded — orphaned object
		// TODO: integrate with structured logging
		_ = err
	}

	// Delete multipart upload record from DB
	if err = h.oss3UC.DeleteMultipartUploadRecord(r.Context(), uploadID); err != nil {
		// TODO: integrate with structured logging
		_ = err
	}

	// Return XML response per S3 spec
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
func (h *OSS3MultipartHandler) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	// TODO: Extract user_id from context
	userID := "user-placeholder"
	requestID := "req-placeholder"

	// Check rate limit
	if _, err := h.oss3UC.CheckRateLimit(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrRateLimitExceeded, r.URL.Path, "")
		return
	}

	// Abort multipart upload in backend
	if err := h.oss3UC.Backend().AbortMultipartUpload(r.Context(), bucket, key, uploadID); err != nil {
		WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete multipart upload record from DB
	if err := h.oss3UC.DeleteMultipartUploadRecord(r.Context(), uploadID); err != nil {
		// TODO: integrate with structured logging
		_ = err
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListParts handles GET /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleListParts(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	// TODO: Extract user_id from context
	userID := "user-placeholder"
	requestID := "req-placeholder"

	// Check rate limit
	if _, err := h.oss3UC.CheckRateLimit(r.Context(), userID, requestID); err != nil {
		WriteS3Error(w, rtcoss3.ErrRateLimitExceeded, r.URL.Path, "")
		return
	}

	// List parts from backend
	parts, err := h.oss3UC.Backend().ListParts(r.Context(), bucket, key, uploadID)
	if err != nil {
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

// Unused import guard
var _ = io.EOF
