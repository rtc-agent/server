package httphandler

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/rtc-agent/server/pkg/logger"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// handleDeleteObject handles DELETE /{bucket}/{key} — delete object.
func (h *OSS3Handler) handleDeleteObject(
	w http.ResponseWriter, r *http.Request, bucket, key string,
) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.delete_object",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("key", key),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Bug #8 fix: Get file record before deletion to release quota.
	// If the record doesn't exist, we still proceed with the backend delete
	// (the file may exist in MinIO without a DB record — reconciliation will handle it).
	var fileSize int64
	file, err := h.oss3UC.GetFileRecord(r.Context(), userID, key)
	if err == nil && file != nil {
		fileSize = file.Size
	}

	// Delete from backend
	if err := h.oss3UC.Backend().DeleteObject(r.Context(), bucket, key); err != nil {
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete file record from DB with retry logic for compensation
	h.deleteFileRecordWithRetry(r.Context(), userID, key)

	// Bug #8 fix: Adjust quota after successful deletion.
	// OSS3-27 fix: Compensation logic moved to usecase layer.
	h.oss3UC.ReleaseQuotaForDeletedFile(r.Context(), userID, key, fileSize)

	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteObjects handles POST /{bucket}?delete= — batch delete objects (H4).
// S3 spec: https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html
func (h *OSS3Handler) handleDeleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx, span := otel.GetTracerProvider().Tracer("oss3").Start(r.Context(), "oss3.delete_objects",
		trace.WithAttributes(
			attribute.String("bucket", bucket),
			attribute.String("user.id", ExtractUserIDFromContext(r.Context())),
		),
	)
	defer span.End()
	r = r.WithContext(ctx)

	userID := ExtractUserIDFromContext(r.Context())

	// Limit XML request body to prevent memory exhaustion.
	// S3 spec allows up to 1000 keys per request; typical XML is ~50KB.
	r.Body = http.MaxBytesReader(w, r.Body, maxXMLRequestBodySize)

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
		// Check if the error is due to request body being too large
		if strings.Contains(err.Error(), "http: request body too large") {
			rtcoss3.WriteS3Error(w, rtcoss3.ErrEntityTooLarge, r.URL.Path, "")
			return
		}
		rtcoss3.WriteS3Error(w, rtcoss3.ErrMalformedXML, r.URL.Path, "")
		return
	}

	// Validate request
	if len(req.Objects) == 0 {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		return
	}
	if len(req.Objects) > 1000 {
		rtcoss3.WriteS3Error(w, rtcoss3.ErrInvalidRequest, r.URL.Path, "")
		return
	}

	// Validate all keys belong to this user and extract key list
	keys := make([]string, 0, len(req.Objects))
	for _, obj := range req.Objects {
		if err := rtcoss3.ValidateKey(obj.Key, userID); err != nil {
			rtcoss3.WriteS3Error(w, err, r.URL.Path, "")
			return
		}
		keys = append(keys, obj.Key)
	}

	// Batch delete from backend
	results, err := h.oss3UC.Backend().DeleteObjects(r.Context(), bucket, keys)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		rtcoss3.WriteS3Error(w, mapBackendError(err), r.URL.Path, "")
		return
	}

	// Delete file records from DB with retry logic for compensation.
	// This ensures consistency between backend storage and DB records.
	for i, result := range results {
		if result.Code == "" {
			// Successful deletion — delete DB record with retry
			h.deleteFileRecordWithRetry(r.Context(), userID, keys[i])
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

	writeXMLResponse(w, http.StatusOK, resp)
}

// deleteFileRecordWithRetry attempts to delete a file record from the DB with retry logic.
// This compensates for transient DB failures after a successful backend operation.
// If all retries fail, it logs an error and records an orphaned record for manual cleanup.
// Uses cenkalti/backoff for exponential backoff with jitter, consistent with commitQuotaWithRetry.
func (h *OSS3Handler) deleteFileRecordWithRetry(ctx context.Context, userID, key string) {
	if err := h.oss3UC.DeleteFileRecord(ctx, userID, key); err == nil {
		return
	} else {
		logger.Warn(ctx, "failed to delete file record after successful backend delete, retrying",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Error(err))
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 100 * time.Millisecond
	bo.MaxInterval = 2 * time.Second

	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, h.oss3UC.DeleteFileRecord(ctx, userID, key)
	},
		backoff.WithBackOff(bo),
		backoff.WithMaxTries(3),
	)
	if err != nil {
		// All retries failed — log for manual intervention or async cleanup
		logger.Error(ctx, "failed to delete file record after all retries, orphaned record",
			zap.String("user_id", userID),
			zap.String("key", key),
			zap.Error(err))
		RecordOrphanedRecord("delete_failed")
	}
}
