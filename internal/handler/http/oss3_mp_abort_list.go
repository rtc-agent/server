package httphandler

import (
	"encoding/xml"
	"net/http"
	"strconv"

	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleAbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleAbortMultipartUpload(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(
		r.Context(), "oss3.abort_multipart_upload",
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

	// Abort multipart upload in backend
	if err := h.oss3UC.Backend().AbortMultipartUpload(r.Context(), bucket, key, uploadID); err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete multipart upload record from DB
	if err := h.oss3UC.DeleteMultipartUploadRecord(r.Context(), uploadID); err != nil {
		logger.Warn(r.Context(), "failed to delete multipart upload record after abort",
			zap.String("upload_id", uploadID),
			zap.Error(err))
	}

	RecordMultipartUploadEnd()

	w.WriteHeader(http.StatusNoContent)
}

// handleListParts handles GET /{bucket}/{key}?uploadId={id}
func (h *OSS3MultipartHandler) handleListParts(
	w http.ResponseWriter, r *http.Request,
	bucket, key, uploadID string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.list_parts",
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

	// Parse pagination parameters
	const maxPartsPerPage = 1000
	partNumberMarker := 0
	if marker := r.URL.Query().Get("part-number-marker"); marker != "" {
		if parsed, err := strconv.Atoi(marker); err == nil && parsed >= 0 {
			partNumberMarker = parsed
		}
	}
	maxParts := maxPartsPerPage
	if mp := r.URL.Query().Get("max-parts"); mp != "" {
		if parsed, err := strconv.Atoi(mp); err == nil && parsed > 0 && parsed <= maxPartsPerPage {
			maxParts = parsed
		}
	}

	// List all parts from backend (backend handles its own pagination internally)
	allParts, err := h.oss3UC.Backend().ListParts(r.Context(), bucket, key, uploadID)
	if err != nil {
		span.SetStatus(codes.Error, "backend_error")
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Paginate: find parts after partNumberMarker
	var paginatedParts []rtcoss3.PartInfo
	for _, p := range allParts {
		if p.PartNumber > partNumberMarker {
			paginatedParts = append(paginatedParts, p)
			if len(paginatedParts) >= maxParts {
				break
			}
		}
	}

	isTruncated := false
	nextPartNumberMarker := 0
	if len(paginatedParts) > 0 {
		lastPart := paginatedParts[len(paginatedParts)-1]
		nextPartNumberMarker = lastPart.PartNumber
		// Check if there are more parts after the last returned one
		for _, p := range allParts {
			if p.PartNumber > lastPart.PartNumber {
				isTruncated = true
				break
			}
		}
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
		PartNumberMarker     int       `xml:"PartNumberMarker"`
		NextPartNumberMarker int       `xml:"NextPartNumberMarker"`
	}

	resp := xmlListPartsResult{
		Bucket:               bucket,
		Key:                  key,
		UploadID:             uploadID,
		MaxParts:             maxParts,
		PartNumberMarker:     partNumberMarker,
		NextPartNumberMarker: nextPartNumberMarker,
		IsTruncated:          isTruncated,
	}

	for _, p := range paginatedParts {
		resp.Parts = append(resp.Parts, xmlPart{
			PartNumber:   p.PartNumber,
			LastModified: p.LastModified.UTC().Format(rtcoss3.S3TimeFormat),
			ETag:         p.ETag,
			Size:         p.Size,
		})
	}

	writeXMLResponse(w, http.StatusOK, resp)
}
