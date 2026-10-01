package httphandler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewOSS3MetricsMiddleware_RecordsRequest(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	handler := NewOSS3MetricsMiddleware()(inner)

	req := httptest.NewRequest(http.MethodGet, "/rtc-agent/user-123/file.txt", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello", rec.Body.String())
}

func TestNewOSS3MetricsMiddleware_SkipsOPTIONS(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := NewOSS3MetricsMiddleware()(inner)

	req := httptest.NewRequest(http.MethodOptions, "/rtc-agent/user-123/file.txt", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.True(t, called, "inner handler should be called for OPTIONS")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestExtractOperationFromMetrics(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		query    string
		copySrc  string
		expected string
	}{
		{"PutObject", http.MethodPut, "/bucket/key", "", "", "PutObject"},
		{"GetObject", http.MethodGet, "/bucket/key", "", "", "GetObject"},
		{"DeleteObject", http.MethodDelete, "/bucket/key", "", "", "DeleteObject"},
		{"HeadObject", http.MethodHead, "/bucket/key", "", "", "HeadObject"},
		{"ListObjects", http.MethodGet, "/bucket", "", "", "ListObjects"},
		{"CopyObject", http.MethodPut, "/bucket/key", "", "src-bucket/src-key", "CopyObject"},
		{"CreateMultipartUpload", http.MethodPost, "/bucket/key", "uploads", "", "CreateMultipartUpload"},
		{"UploadPart", http.MethodPut, "/bucket/key", "uploadId=abc", "", "UploadPart"},
		{"CompleteMultipartUpload", http.MethodPost, "/bucket/key", "uploadId=abc", "", "CompleteMultipartUpload"},
		{"AbortMultipartUpload", http.MethodDelete, "/bucket/key", "uploadId=abc", "", "AbortMultipartUpload"},
		{"ListParts", http.MethodGet, "/bucket/key", "uploadId=abc", "", "ListParts"},
		{"Unknown", http.MethodPatch, "/bucket/key", "", "", "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.query != "" {
				req.URL.RawQuery = tt.query
			}
			if tt.copySrc != "" {
				req.Header.Set("X-Amz-Copy-Source", tt.copySrc)
			}
			result := extractOperation(req)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRecordMultipartUploadStartEnd(t *testing.T) {
	// These should not panic
	RecordMultipartUploadStart("user-123")
	RecordMultipartUploadEnd("user-123")
}

func TestRecordBackendError(t *testing.T) {
	// Should not panic
	RecordBackendError("PutObject", "disk_full")
}

func TestRecordQuotaUsage(t *testing.T) {
	// Should not panic
	RecordQuotaUsage("user-123", 1024*1024)
}
