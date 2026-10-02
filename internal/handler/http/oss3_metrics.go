package httphandler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// OSS3 Prometheus metrics.
//
// These metrics are separate from the main HTTP metrics (http_requests_total, etc.)
// to allow OSS3-specific alerting and dashboarding.
var (
	// oss3UploadBytesTotal tracks total bytes uploaded per bucket.
	oss3UploadBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_upload_bytes_total",
			Help: "Total bytes uploaded to OSS3",
		},
		[]string{"bucket"},
	)

	// oss3DownloadBytesTotal tracks total bytes downloaded per bucket.
	oss3DownloadBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_download_bytes_total",
			Help: "Total bytes downloaded from OSS3",
		},
		[]string{"bucket"},
	)

	// oss3RequestsTotal tracks total S3 requests by operation and status.
	oss3RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_requests_total",
			Help: "Total OSS3 S3 requests",
		},
		[]string{"operation", "status"},
	)

	// oss3RequestDuration tracks S3 request latency by operation.
	oss3RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rtc_oss3_request_duration_seconds",
			Help:    "OSS3 S3 request duration in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 15),
		},
		[]string{"operation"},
	)

	// oss3MultipartUploadsActive tracks active multipart uploads.
	oss3MultipartUploadsActive = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rtc_oss3_multipart_uploads_active",
			Help: "Active multipart uploads",
		},
		[]string{},
	)

	// oss3QuotaUsageBytes tracks current quota usage.
	oss3QuotaUsageBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rtc_oss3_quota_usage_bytes",
			Help: "Current quota usage in bytes",
		},
		[]string{},
	)

	// oss3BackendErrorsTotal tracks backend errors by operation and error type.
	oss3BackendErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_backend_errors_total",
			Help: "Total backend errors",
		},
		[]string{"operation", "error_type"},
	)

	// oss3InstantUploadTotal tracks instant upload hits by type.
	// Types: db_hit (both DB and MinIO confirm), minio_repair (MinIO hit, DB repaired),
	// multipart (multipart upload instant hit).
	oss3InstantUploadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_instant_upload_total",
			Help: "Total instant upload hits by type",
		},
		[]string{"type"},
	)

	// oss3InstantUploadRepairErrorTotal tracks DB repair failures during instant upload.
	// Incremented when MinIO has the file but the DB record repair fails.
	oss3InstantUploadRepairErrorTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "rtc_oss3_instant_upload_repair_error_total",
			Help: "Total instant upload DB repair failures",
		},
	)

	// oss3ConsistencyViolationTotal tracks cases where DB and MinIO are inconsistent.
	// Incremented when DB has a record but MinIO is missing the file, or vice versa.
	oss3ConsistencyViolationTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_consistency_violation_total",
			Help: "Total DB/MinIO consistency violations",
		},
		[]string{"type"}, // "db_has_minio_missing", "minio_has_db_missing"
	)

	// LOW-18 fix: oss3OrphanedRecordTotal tracks orphaned records that failed to delete.
	// Incremented when backend delete succeeds but DB delete fails after all retries.
	oss3OrphanedRecordTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_orphaned_record_total",
			Help: "Total orphaned records (backend deleted, DB delete failed)",
		},
		[]string{"operation"}, // operation: "delete_failed", "copy_failed"
	)

	// oss3OrphanedQuotaCommitTotal tracks quota commits that failed after all retries
	// while the upload succeeded. These require reconciliation to sync Redis with DB.
	oss3OrphanedQuotaCommitTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_orphaned_quota_commits_total",
			Help: "Total quota commits that failed after retries (upload succeeded, reconciliation needed)",
		},
		[]string{},
	)

	// oss3QuotaDriftBytes tracks the absolute drift between Redis quota counter
	// and DB truth, updated by ReconcileQuota.
	oss3QuotaDriftBytes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "rtc_oss3_quota_drift_bytes",
			Help: "Absolute drift between Redis quota counter and DB truth (latest reconciliation)",
		},
	)

	// oss3QuotaCommitRetryTotal tracks the number of quota commit retries.
	oss3QuotaCommitRetryTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "rtc_oss3_quota_commit_retry_total",
			Help: "Total quota commit retries (transient Redis errors)",
		},
	)
)

// NewOSS3MetricsMiddleware creates a Prometheus metrics middleware for OSS3.
//
// Uses github.com/felixge/httpsnoop to transparently wrap http.ResponseWriter,
// preserving Hijacker/Flusher/Pusher interfaces for streaming responses.
//
// Captured metrics:
//   - rtc_oss3_requests_total          request count (grouped by operation/status)
//   - rtc_oss3_request_duration_seconds request latency distribution
//   - rtc_oss3_upload_bytes_total       upload bytes per bucket
//   - rtc_oss3_download_bytes_total     download bytes per bucket
func NewOSS3MetricsMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip OPTIONS preflight requests (CORS)
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()

			// Use httpsnoop to capture status code and bytes written
			// while preserving all ResponseWriter interfaces
			wrapped := httpsnoop.CaptureMetrics(next, w, r)

			duration := time.Since(start).Seconds()
			operation := extractOperation(r)
			statusStr := strconv.Itoa(wrapped.Code)

			// Record request count and duration
			oss3RequestsTotal.WithLabelValues(operation, statusStr).Inc()
			oss3RequestDuration.WithLabelValues(operation).Observe(duration)

			// Record upload/download bytes
			bucket, _, _ := parseS3Path(r.URL.Path)

			if r.Method == http.MethodPut && wrapped.Code >= 200 && wrapped.Code < 300 {
				if bucket != "" {
					oss3UploadBytesTotal.WithLabelValues(bucket).Add(float64(wrapped.Written))
				}
			}
			if r.Method == http.MethodGet && wrapped.Code >= 200 && wrapped.Code < 300 {
				if bucket != "" {
					oss3DownloadBytesTotal.WithLabelValues(bucket).Add(float64(wrapped.Written))
				}
			}
		})
	}
}

// RecordMultipartUploadStart increments the active multipart uploads gauge.
// Called when a CreateMultipartUpload request succeeds.
func RecordMultipartUploadStart() {
	oss3MultipartUploadsActive.WithLabelValues().Inc()
}

// RecordMultipartUploadEnd decrements the active multipart uploads gauge.
// Called when a multipart upload is completed or aborted.
func RecordMultipartUploadEnd() {
	oss3MultipartUploadsActive.WithLabelValues().Dec()
}

// RecordBackendError increments the backend error counter for the given operation and error type.
// Used for alerting on storage backend failures.
func RecordBackendError(operation, errorType string) {
	oss3BackendErrorsTotal.WithLabelValues(operation, errorType).Inc()
}

// RecordQuotaUsage sets the current quota usage gauge.
// Called after reconciliation to reflect the corrected quota value.
func RecordQuotaUsage(bytes int64) {
	oss3QuotaUsageBytes.WithLabelValues().Set(float64(bytes))
}

// RecordInstantUpload increments the instant upload hit counter for the given type.
// Types: "db_hit" (both DB and MinIO confirm), "minio_repair" (MinIO hit, DB repaired),
// "multipart" (multipart upload instant hit).
func RecordInstantUpload(uploadType string) {
	oss3InstantUploadTotal.WithLabelValues(uploadType).Inc()
}

// RecordInstantUploadRepairError increments the counter for DB repair failures
// during instant upload. Called when MinIO has the file but the DB record repair fails.
func RecordInstantUploadRepairError() {
	oss3InstantUploadRepairErrorTotal.Inc()
}

// RecordConsistencyViolation increments the consistency violation counter for the given type.
// Types: "db_has_minio_missing" (DB record exists but MinIO file missing),
// "minio_has_db_missing" (MinIO has file but DB record missing).
func RecordConsistencyViolation(violationType string) {
	oss3ConsistencyViolationTotal.WithLabelValues(violationType).Inc()
}

// RecordOrphanedRecord increments the orphaned record counter for the given operation.
// Called when backend delete succeeds but DB delete fails after all retries.
func RecordOrphanedRecord(operation string) {
	oss3OrphanedRecordTotal.WithLabelValues(operation).Inc()
}

// RecordOrphanedQuotaCommit increments the orphaned quota commit counter.
// Called when quota commit fails after all retries while the upload succeeded.
// Reconciliation will eventually fix the drift.
func RecordOrphanedQuotaCommit() {
	oss3OrphanedQuotaCommitTotal.WithLabelValues().Inc()
}

// RecordQuotaCommitRetry increments the quota commit retry counter.
// Called each time a transient Redis error triggers a retry during quota commit.
func RecordQuotaCommitRetry() {
	oss3QuotaCommitRetryTotal.Inc()
}
