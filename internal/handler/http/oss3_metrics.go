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
	// oss3UploadBytesTotal tracks total bytes uploaded per user/bucket.
	oss3UploadBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_upload_bytes_total",
			Help: "Total bytes uploaded to OSS3",
		},
		[]string{"user_id", "bucket"},
	)

	// oss3DownloadBytesTotal tracks total bytes downloaded per user/bucket.
	oss3DownloadBytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_download_bytes_total",
			Help: "Total bytes downloaded from OSS3",
		},
		[]string{"user_id", "bucket"},
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

	// oss3MultipartUploadsActive tracks active multipart uploads per user.
	oss3MultipartUploadsActive = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rtc_oss3_multipart_uploads_active",
			Help: "Active multipart uploads",
		},
		[]string{"user_id"},
	)

	// oss3QuotaUsageBytes tracks current quota usage per user.
	oss3QuotaUsageBytes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rtc_oss3_quota_usage_bytes",
			Help: "Current quota usage per user in bytes",
		},
		[]string{"user_id"},
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
		[]string{"operation", "user_id"}, // operation: "delete_failed"
	)

	// oss3OrphanedQuotaCommitTotal tracks quota commits that failed after all retries
	// while the upload succeeded. These require reconciliation to sync Redis with DB.
	oss3OrphanedQuotaCommitTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rtc_oss3_orphaned_quota_commits_total",
			Help: "Total quota commits that failed after retries (upload succeeded, reconciliation needed)",
		},
		[]string{"user_id"},
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
//   - rtc_oss3_upload_bytes_total       upload bytes per user/bucket
//   - rtc_oss3_download_bytes_total     download bytes per user/bucket
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
			userID := ExtractUserIDFromContext(r.Context())
			bucket, _, _ := parseS3Path(r.URL.Path)

			if r.Method == http.MethodPut && wrapped.Code >= 200 && wrapped.Code < 300 {
				if userID != "" && bucket != "" {
					oss3UploadBytesTotal.WithLabelValues(userID, bucket).Add(float64(wrapped.Written))
				}
			}
			if r.Method == http.MethodGet && wrapped.Code >= 200 && wrapped.Code < 300 {
				if userID != "" && bucket != "" {
					oss3DownloadBytesTotal.WithLabelValues(userID, bucket).Add(float64(wrapped.Written))
				}
			}
		})
	}
}

// RecordMultipartUploadStart records the start of a multipart upload.
func RecordMultipartUploadStart(userID string) {
	oss3MultipartUploadsActive.WithLabelValues(userID).Inc()
}

// RecordMultipartUploadEnd records the completion or abort of a multipart upload.
func RecordMultipartUploadEnd(userID string) {
	oss3MultipartUploadsActive.WithLabelValues(userID).Dec()
}

// RecordBackendError records a backend error.
func RecordBackendError(operation, errorType string) {
	oss3BackendErrorsTotal.WithLabelValues(operation, errorType).Inc()
}

// RecordQuotaUsage updates the quota usage gauge for a user.
func RecordQuotaUsage(userID string, bytes int64) {
	oss3QuotaUsageBytes.WithLabelValues(userID).Set(float64(bytes))
}

// RecordInstantUpload records an instant upload hit.
// Types: "db_hit", "minio_repair", "multipart".
func RecordInstantUpload(uploadType string) {
	oss3InstantUploadTotal.WithLabelValues(uploadType).Inc()
}

// RecordInstantUploadRepairError records a DB repair failure during instant upload.
func RecordInstantUploadRepairError() {
	oss3InstantUploadRepairErrorTotal.Inc()
}

// RecordConsistencyViolation records a DB/MinIO consistency violation.
// Types: "db_has_minio_missing", "minio_has_db_missing".
func RecordConsistencyViolation(violationType string) {
	oss3ConsistencyViolationTotal.WithLabelValues(violationType).Inc()
}

// RecordOrphanedRecord records an orphaned record that failed to delete.
// LOW-18 fix: Called when backend delete succeeds but DB delete fails after all retries.
func RecordOrphanedRecord(operation, userID string) {
	oss3OrphanedRecordTotal.WithLabelValues(operation, userID).Inc()
}

// RecordOrphanedQuotaCommit records a quota commit that failed after all retries
// while the upload succeeded. Reconciliation will eventually fix the drift.
func RecordOrphanedQuotaCommit(userID string) {
	oss3OrphanedQuotaCommitTotal.WithLabelValues(userID).Inc()
}

// RecordQuotaCommitRetry records a single quota commit retry attempt.
func RecordQuotaCommitRetry() {
	oss3QuotaCommitRetryTotal.Inc()
}

// RecordQuotaDrift records the absolute drift between Redis and DB quota values.
// Called by ReconcileQuota after each reconciliation cycle.
func RecordQuotaDrift(driftBytes int64) {
	oss3QuotaDriftBytes.Set(float64(driftBytes))
}
