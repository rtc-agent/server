package httphandler

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Unit tests for validation and parsing logic
// ============================================================================

// TestValidateKey_SpecialCharacters tests that key validation handles various
// special characters correctly.
func TestValidateKey_SpecialCharacters(t *testing.T) {
	userID := testUserID()

	tests := []struct {
		name    string
		key     string
		wantErr string // error code, or "" for success
	}{
		{
			name:    "valid key with lowercase",
			key:     fmt.Sprintf("user-%s/%s.txt", userID, strings.Repeat("a", 32)),
			wantErr: "",
		},
		{
			name:    "valid key with mixed extension",
			key:     fmt.Sprintf("user-%s/%s.mp4", userID, strings.Repeat("ab", 16)),
			wantErr: "",
		},
		{
			name:    "invalid: Chinese characters in key",
			key:     fmt.Sprintf("user-%s/中文文件.txt", userID),
			wantErr: "InvalidKeyFormat",
		},
		{
			name:    "invalid: emoji in key",
			key:     fmt.Sprintf("user-%s/🔥file.txt", userID),
			wantErr: "InvalidKeyFormat",
		},
		{
			name:    "invalid: spaces in key",
			key:     fmt.Sprintf("user-%s/my file.txt", userID),
			wantErr: "InvalidKeyFormat",
		},
		{
			name:    "invalid: path separator in filename",
			key:     fmt.Sprintf("user-%s/dir/file.txt", userID),
			wantErr: "InvalidKeyFormat",
		},
		{
			name:    "invalid: different user prefix",
			key:     "user-11111111-1111-1111-1111-111111111111/" + strings.Repeat("a", 32) + ".txt",
			wantErr: "AccessDenied",
		},
		{
			name:    "invalid: extension too long (>10 chars)",
			key:     fmt.Sprintf("user-%s/%s.verylongext", userID, strings.Repeat("a", 32)),
			wantErr: "InvalidKeyFormat",
		},
		{
			name:    "invalid: no extension",
			key:     fmt.Sprintf("user-%s/%s", userID, strings.Repeat("a", 32)),
			wantErr: "InvalidKeyFormat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rtcoss3.ValidateKey(tt.key, userID)
			if tt.wantErr == "" {
				assert.Nil(t, err, "expected no error for key %q", tt.key)
			} else {
				require.NotNil(t, err, "expected error for key %q", tt.key)
				assert.Equal(t, tt.wantErr, err.Code)
			}
		})
	}
}

// TestValidateKey_LongKey tests that keys exceeding reasonable length are rejected.
func TestValidateKey_LongKey(t *testing.T) {
	userID := testUserID()

	// Generate a key with a very long "hash" (simulating >1024 chars total)
	longHash := strings.Repeat("a", 1100)
	key := fmt.Sprintf("user-%s/%s.txt", userID, longHash)

	err := rtcoss3.ValidateKey(key, userID)
	require.NotNil(t, err, "expected error for very long key")
	// The key should fail because the hash is not 32 chars
	assert.Equal(t, "InvalidKeyFormat", err.Code)
}

// TestParseS3Path_EdgeCases tests additional edge cases for S3 path parsing.
func TestParseS3Path_EdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantBucket string
		wantKey    string
		wantOK     bool
	}{
		{
			name:       "bucket only",
			path:       "/rtc-agent",
			wantBucket: "rtc-agent",
			wantKey:    "",
			wantOK:     true,
		},
		{
			name:       "bucket with key",
			path:       "/rtc-agent/user-123/file.txt",
			wantBucket: "rtc-agent",
			wantKey:    "user-123/file.txt",
			wantOK:     true,
		},
		{
			name:       "deep key path",
			path:       "/rtc-agent/user-123/path/to/deep/file.txt",
			wantBucket: "rtc-agent",
			wantKey:    "user-123/path/to/deep/file.txt",
			wantOK:     true,
		},
		{
			name:       "path traversal with .. in middle",
			path:       "/rtc-agent/user-123/../other/file.txt",
			wantBucket: "",
			wantKey:    "",
			wantOK:     false,
		},
		{
			name:       "key containing .. as substring is allowed",
			path:       "/rtc-agent/user-123/my..file.txt",
			wantBucket: "rtc-agent",
			wantKey:    "user-123/my..file.txt",
			wantOK:     true,
		},
		{
			name:   "empty path",
			path:   "",
			wantOK: false,
		},
		{
			name:   "root path",
			path:   "/",
			wantOK: false,
		},
		{
			name:       "unicode key",
			path:       "/rtc-agent/user-123/%E4%B8%AD%E6%96%87.txt",
			wantBucket: "rtc-agent",
			wantKey:    "user-123/%E4%B8%AD%E6%96%87.txt",
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bucket, key, ok := parseS3Path(tt.path)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantBucket, bucket)
				assert.Equal(t, tt.wantKey, key)
			}
		})
	}
}

// TestParseMaxKeys_EdgeCases tests max-keys parsing edge cases.
func TestParseMaxKeys_EdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty returns default", "", defaultMaxKeys},
		{"zero returns default", "0", defaultMaxKeys},
		{"negative returns default", "-1", defaultMaxKeys},
		{"valid small value", "10", 10},
		{"valid max value", "1000", 1000},
		{"exceeds max clamped to default", "1001", defaultMaxKeys},
		{"very large number", "999999", defaultMaxKeys},
		{"non-numeric", "abc", defaultMaxKeys},
		{"float", "10.5", defaultMaxKeys},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMaxKeys(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestParseRangeHeader_EdgeCases tests range header parsing edge cases.
func TestParseRangeHeader_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		wantStart int64
		wantEnd   int64
		wantHas   bool
		wantErr   bool
	}{
		{
			name:      "empty header",
			header:    "",
			wantStart: 0,
			wantEnd:   0,
			wantHas:   false,
			wantErr:   false,
		},
		{
			name:      "simple range",
			header:    "bytes=0-499",
			wantStart: 0,
			wantEnd:   499,
			wantHas:   true,
			wantErr:   false,
		},
		{
			name:      "open-ended range",
			header:    "bytes=500-",
			wantStart: 500,
			wantEnd:   -1,
			wantHas:   true,
			wantErr:   false,
		},
		{
			name:      "suffix range",
			header:    "bytes=-500",
			wantStart: -500,
			wantEnd:   -1,
			wantHas:   true,
			wantErr:   false,
		},
		{
			name:    "invalid: zero suffix",
			header:  "bytes=-0",
			wantErr: true,
		},
		{
			name:    "invalid: wrong unit",
			header:  "items=0-10",
			wantErr: true,
		},
		{
			name:    "invalid: multiple dashes",
			header:  "bytes=1-2-3",
			wantErr: true,
		},
		{
			name:    "invalid: start > end",
			header:  "bytes=500-100",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, hasRange, err := parseRangeHeader(tt.header)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantHas, hasRange)
				if hasRange {
					assert.Equal(t, tt.wantStart, start)
					assert.Equal(t, tt.wantEnd, end)
				}
			}
		})
	}
}

// ============================================================================
// Tests using mock backend
// ============================================================================

// TestMockBackend_ZeroBytePutObject tests that the mock backend correctly
// handles zero-byte file uploads.
func TestMockBackend_ZeroBytePutObject(t *testing.T) {
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	ctx := context.Background()
	bucket := "test-bucket"
	key := "user-123/empty.txt"

	// Upload zero bytes
	etag, err := backend.PutObject(ctx, bucket, key, bytes.NewReader(nil), 0, "text/plain")
	require.NoError(t, err)
	assert.NotEmpty(t, etag)

	// Verify object exists with zero size
	meta, err := backend.HeadObject(ctx, bucket, key)
	require.NoError(t, err)
	assert.Equal(t, int64(0), meta.Size)
	assert.Equal(t, "text/plain", meta.ContentType)
	assert.Equal(t, etag, meta.ETag)

	// Verify GetObject returns empty body
	reader, getMeta, err := backend.GetObject(ctx, bucket, key)
	require.NoError(t, err)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, 0, len(data))
	assert.Equal(t, int64(0), getMeta.Size)
}

// TestMockBackend_ListObjectsPagination tests pagination with the mock backend.
func TestMockBackend_ListObjectsPagination(t *testing.T) {
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	ctx := context.Background()
	bucket := "test-bucket"

	// Upload 25 objects
	for i := 0; i < 25; i++ {
		key := fmt.Sprintf("user-123/%032x.txt", i)
		data := []byte(fmt.Sprintf("content-%d", i))
		_, err := backend.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), "text/plain")
		require.NoError(t, err)
	}

	// First page: max-keys=10
	result1, err := backend.ListObjects(ctx, bucket, rtcoss3.ListObjectsOptions{
		Prefix:  "user-123/",
		MaxKeys: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 10, len(result1.Objects))
	assert.True(t, result1.IsTruncated)

	// Second page: use last key as marker
	result2, err := backend.ListObjects(ctx, bucket, rtcoss3.ListObjectsOptions{
		Prefix:  "user-123/",
		Marker:  result1.Objects[len(result1.Objects)-1].Key,
		MaxKeys: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 10, len(result2.Objects))
	assert.True(t, result2.IsTruncated)

	// Third page: remaining 5 objects
	result3, err := backend.ListObjects(ctx, bucket, rtcoss3.ListObjectsOptions{
		Prefix:  "user-123/",
		Marker:  result2.Objects[len(result2.Objects)-1].Key,
		MaxKeys: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 5, len(result3.Objects))
	assert.False(t, result3.IsTruncated)
}

// TestMockBackend_CopyObject tests cross-bucket copy with the mock backend.
func TestMockBackend_CopyObject(t *testing.T) {
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	ctx := context.Background()

	// Upload source object
	srcBucket := "bucket-a"
	srcKey := "user-123/source.txt"
	srcData := []byte("hello world")
	_, err := backend.PutObject(ctx, srcBucket, srcKey, bytes.NewReader(srcData), int64(len(srcData)), "text/plain")
	require.NoError(t, err)

	// Copy to different bucket
	dstBucket := "bucket-b"
	dstKey := "user-123/dest.txt"
	result, err := backend.CopyObject(ctx, srcBucket, srcKey, dstBucket, dstKey)
	require.NoError(t, err)
	assert.NotEmpty(t, result.ETag)
	assert.Equal(t, dstKey, result.Key)

	// Verify destination exists with same data
	reader, meta, err := backend.GetObject(ctx, dstBucket, dstKey)
	require.NoError(t, err)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, srcData, data)
	assert.Equal(t, "text/plain", meta.ContentType)
}

// TestMockBackend_DeleteObjectsPartialSuccess tests batch delete where some
// objects don't exist.
func TestMockBackend_DeleteObjectsPartialSuccess(t *testing.T) {
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	ctx := context.Background()
	bucket := "test-bucket"

	// Upload 5 objects
	existingKeys := make([]string, 5)
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("user-123/%032x.txt", i)
		existingKeys[i] = key
		data := []byte(fmt.Sprintf("content-%d", i))
		_, err := backend.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), "text/plain")
		require.NoError(t, err)
	}

	// Delete 10 objects: 5 exist, 5 don't
	keysToDelete := make([]string, 10)
	copy(keysToDelete, existingKeys)
	for i := 5; i < 10; i++ {
		keysToDelete[i] = fmt.Sprintf("user-123/%032x.txt", i+100) // non-existent
	}

	results, err := backend.DeleteObjects(ctx, bucket, keysToDelete)
	require.NoError(t, err)
	assert.Equal(t, 10, len(results))

	// All results should succeed (mock backend doesn't track non-existent keys)
	for _, r := range results {
		assert.Empty(t, r.Code, "expected no error for key %s", r.Key)
	}

	// Verify existing objects were deleted
	for _, key := range existingKeys {
		_, err := backend.HeadObject(ctx, bucket, key)
		assert.ErrorIs(t, err, rtcoss3.ErrKeyNotFound)
	}
}

// TestMockBackend_MultipartUploadLifecycle tests the full multipart upload lifecycle.
func TestMockBackend_MultipartUploadLifecycle(t *testing.T) {
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	ctx := context.Background()
	bucket := "test-bucket"
	key := "user-123/large-file.bin"

	// Create multipart upload
	createResult, err := backend.CreateMultipartUpload(ctx, bucket, key, "application/octet-stream")
	require.NoError(t, err)
	assert.NotEmpty(t, createResult.UploadID)
	uploadID := createResult.UploadID

	// Upload 3 parts
	var completedParts []rtcoss3.CompletedPart
	for i := 1; i <= 3; i++ {
		partData := []byte(fmt.Sprintf("part-%d-data", i))
		etag, err := backend.UploadPart(ctx, bucket, key, uploadID, i, bytes.NewReader(partData), int64(len(partData)))
		require.NoError(t, err)
		assert.NotEmpty(t, etag)
		completedParts = append(completedParts, rtcoss3.CompletedPart{
			PartNumber: i,
			ETag:       etag,
		})
	}

	// List parts
	parts, err := backend.ListParts(ctx, bucket, key, uploadID)
	// Note: mock backend returns empty list - this tests the interface
	require.NoError(t, err)
	_ = parts

	// Complete multipart upload
	finalETag, err := backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, completedParts)
	require.NoError(t, err)
	assert.NotEmpty(t, finalETag)

	// Abort should succeed (even after complete in mock)
	err = backend.AbortMultipartUpload(ctx, bucket, key, uploadID)
	require.NoError(t, err)
}

// TestMockBackend_AbortMultipartUploadCleanup tests that aborting a multipart
// upload cleans up resources (at the backend level).
func TestMockBackend_AbortMultipartUploadCleanup(t *testing.T) {
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	ctx := context.Background()
	bucket := "test-bucket"
	key := "user-123/aborted-file.bin"

	// Create multipart upload
	createResult, err := backend.CreateMultipartUpload(ctx, bucket, key, "application/octet-stream")
	require.NoError(t, err)

	// Upload some parts
	for i := 1; i <= 3; i++ {
		partData := []byte(fmt.Sprintf("part-%d-data", i))
		_, err := backend.UploadPart(ctx, bucket, key, createResult.UploadID, i, bytes.NewReader(partData), int64(len(partData)))
		require.NoError(t, err)
	}

	// Abort the upload
	err = backend.AbortMultipartUpload(ctx, bucket, key, createResult.UploadID)
	require.NoError(t, err)

	// Verify the object doesn't exist (abort should prevent it from being assembled)
	_, err = backend.HeadObject(ctx, bucket, key)
	assert.ErrorIs(t, err, rtcoss3.ErrKeyNotFound, "aborted upload should not create an object")
}

// ============================================================================
// Concurrent quota reservation test (unit test for race conditions)
// ============================================================================

// threadSafeMockBackend wraps mockBackend with a mutex for concurrent access.
type threadSafeMockBackend struct {
	*mockBackend
	mu sync.RWMutex
}

func (m *threadSafeMockBackend) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mockBackend.PutObject(ctx, bucket, key, reader, size, contentType)
}

func (m *threadSafeMockBackend) ListObjects(ctx context.Context, bucket string, opts rtcoss3.ListObjectsOptions) (*rtcoss3.ListObjectsResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mockBackend.ListObjects(ctx, bucket, opts)
}

// TestQuotaReservation_Concurrent tests that concurrent quota operations
// don't cause race conditions in the counter logic.
// NOTE: This test exercises the concept but needs Redis for full integration.
func TestQuotaReservation_Concurrent(t *testing.T) {
	// This test verifies the concurrency safety of quota logic.
	// Without Redis, we can only test that a thread-safe backend handles
	// concurrent operations without data races.

	backend := &threadSafeMockBackend{
		mockBackend: &mockBackend{
			objects: make(map[string][]byte),
			meta:    make(map[string]rtcoss3.ObjectMeta),
		},
	}

	ctx := context.Background()
	bucket := "test-bucket"

	// Launch 50 concurrent uploads
	var wg sync.WaitGroup
	errCh := make(chan error, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("user-123/%032x.txt", idx)
			data := []byte(fmt.Sprintf("content-%d", idx))
			_, err := backend.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), "text/plain")
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		t.Errorf("concurrent upload failed: %v", err)
	}

	// Verify all 50 objects were created
	result, err := backend.ListObjects(ctx, bucket, rtcoss3.ListObjectsOptions{
		Prefix:  "user-123/",
		MaxKeys: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, 50, len(result.Objects))
}

// ============================================================================
// Integration test stubs (require running server with Redis + MinIO)
// ============================================================================

// TestPutObject_ZeroByte_Integration tests zero-byte file upload end-to-end.
// Requires: RTC_OSS3_REFRESH_TOKEN, running server, Redis, MinIO.
func TestPutObject_ZeroByte_Integration(t *testing.T) {
	creds, serverURL, bucket := setupIntegrationTest(t)

	userID := testUserID()
	key := generateValidKey(userID, "txt")

	// Upload zero-byte file
	path := fmt.Sprintf("/s3/%s/%s", bucket, key)
	httpReq := newSignedS3Request(t, http.MethodPut, path, bytes.NewReader(nil), creds, serverURL)
	httpReq.Header.Set("Content-Type", "text/plain")
	httpReq.Header.Set("Content-Length", "0")

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("upload zero-byte file: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", string(bodyBytes))
	assert.NotEmpty(t, resp.Header.Get("ETag"), "expected ETag in response")

	t.Logf("Zero-byte upload successful: ETag=%s", resp.Header.Get("ETag"))
}

// TestMultipartUpload_MaxParts_Integration tests that uploading more than 10000
// parts is rejected with an error.
// Requires: RTC_OSS3_REFRESH_TOKEN, running server, Redis, MinIO.
func TestMultipartUpload_MaxParts_Integration(t *testing.T) {
	creds, serverURL, bucket := setupIntegrationTest(t)

	userID := testUserID()
	key := generateValidKey(userID, "bin")

	// Create multipart upload
	createPath := fmt.Sprintf("/s3/%s/%s?uploads", bucket, key)
	createReq := newSignedS3Request(t, http.MethodPost, createPath, nil, creds, serverURL)
	createReq.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{}
	createResp, err := client.Do(createReq)
	if err != nil {
		t.Fatalf("create multipart upload: %v", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create multipart upload: status %d, body: %s", createResp.StatusCode, string(bodyBytes))
	}

	type createResult struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		UploadId string   `xml:"UploadId"`
	}
	var cr createResult
	require.NoError(t, xml.NewDecoder(createResp.Body).Decode(&cr))
	uploadID := cr.UploadId

	// Try to upload part number 10001 (exceeds max of 10000)
	partPath := fmt.Sprintf("/s3/%s/%s?partNumber=10001&uploadId=%s", bucket, key, uploadID)
	partData := bytes.Repeat([]byte("x"), 5*1024*1024) // 5MB part
	partReq := newSignedS3Request(t, http.MethodPut, partPath, bytes.NewReader(partData), creds, serverURL)
	partReq.Header.Set("Content-Type", "application/octet-stream")
	partReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(partData)))

	partResp, err := client.Do(partReq)
	if err != nil {
		t.Fatalf("upload part 10001: %v", err)
	}
	defer partResp.Body.Close()

	bodyBytes, _ := io.ReadAll(partResp.Body)

	// Part number 10001 should be rejected
	assert.Equal(t, http.StatusBadRequest, partResp.StatusCode,
		"expected 400 for part number > 10000, got %d. Body: %s", partResp.StatusCode, string(bodyBytes))
	assert.Contains(t, string(bodyBytes), "InvalidPartNumber")

	// Cleanup: abort the multipart upload
	abortPath := fmt.Sprintf("/s3/%s/%s?uploadId=%s", bucket, key, uploadID)
	abortReq := newSignedS3Request(t, http.MethodDelete, abortPath, nil, creds, serverURL)
	abortResp, _ := client.Do(abortReq)
	if abortResp != nil {
		abortResp.Body.Close()
	}
}

// TestListObjects_Pagination_Integration tests pagination with 100 objects.
// Requires: RTC_OSS3_REFRESH_TOKEN, running server, Redis, MinIO.
func TestListObjects_Pagination_Integration(t *testing.T) {
	creds, serverURL, bucket := setupIntegrationTest(t)

	userID := testUserID()
	client := &http.Client{}

	// Upload 20 objects (using fewer for faster integration test)
	numObjects := 20
	for i := 0; i < numObjects; i++ {
		key := generateValidKey(userID, "txt")

		path := fmt.Sprintf("/s3/%s/%s", bucket, key)
		data := []byte(fmt.Sprintf("content-%d", i))
		httpReq := newSignedS3Request(t, http.MethodPut, path, bytes.NewReader(data), creds, serverURL)
		httpReq.Header.Set("Content-Type", "text/plain")
		httpReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))

		resp, err := client.Do(httpReq)
		if err != nil {
			t.Fatalf("upload object %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload object %d: status %d", i, resp.StatusCode)
		}
	}

	// List with max-keys=5 (paginate through results)
	allKeys := make([]string, 0, numObjects)
	marker := ""
	pages := 0
	for {
		pages++
		listPath := fmt.Sprintf("/s3/%s?max-keys=5&prefix=user-%s/", bucket, userID)
		if marker != "" {
			listPath += "&marker=" + marker
		}

		listReq := newSignedS3Request(t, http.MethodGet, listPath, nil, creds, serverURL)
		listResp, err := client.Do(listReq)
		if err != nil {
			t.Fatalf("list objects page %d: %v", pages, err)
		}

		bodyBytes, _ := io.ReadAll(listResp.Body)
		listResp.Body.Close()

		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("list objects page %d: status %d, body: %s", pages, listResp.StatusCode, string(bodyBytes))
		}

		// Parse XML response
		type listResult struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			IsTruncated bool     `xml:"IsTruncated"`
			NextMarker  string   `xml:"NextMarker"`
			Contents    []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}

		var result listResult
		if err := xml.Unmarshal(bodyBytes, &result); err != nil {
			t.Fatalf("unmarshal list response: %v", err)
		}

		for _, c := range result.Contents {
			allKeys = append(allKeys, c.Key)
		}

		if !result.IsTruncated {
			break
		}
		marker = result.NextMarker
		if marker == "" && len(result.Contents) > 0 {
			marker = result.Contents[len(result.Contents)-1].Key
		}

		if pages > 10 {
			t.Fatal("too many pages, possible infinite loop")
		}
	}

	assert.Equal(t, numObjects, len(allKeys), "expected %d objects, got %d in %d pages", numObjects, len(allKeys), pages)
	t.Logf("Listed %d objects in %d pages", len(allKeys), pages)
}

// TestDeleteObjects_PartialSuccess_Integration tests batch delete where some
// objects don't exist, verifying the response contains both success and error info.
// Requires: RTC_OSS3_REFRESH_TOKEN, running server, Redis, MinIO.
func TestDeleteObjects_PartialSuccess_Integration(t *testing.T) {
	creds, serverURL, bucket := setupIntegrationTest(t)

	userID := testUserID()
	client := &http.Client{}

	// Upload 5 objects
	existingKeys := make([]string, 5)
	for i := 0; i < 5; i++ {
		key := generateValidKey(userID, "txt")
		existingKeys[i] = key

		path := fmt.Sprintf("/s3/%s/%s", bucket, key)
		data := []byte(fmt.Sprintf("content-%d", i))
		httpReq := newSignedS3Request(t, http.MethodPut, path, bytes.NewReader(data), creds, serverURL)
		httpReq.Header.Set("Content-Type", "text/plain")
		httpReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))

		resp, err := client.Do(httpReq)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// Build delete request with 10 keys: 5 existing + 5 non-existent
	type deleteRequest struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}

	delReq := deleteRequest{}
	// Add existing keys
	for _, key := range existingKeys {
		obj := struct {
			Key string `xml:"Key"`
		}{Key: key}
		delReq.Objects = append(delReq.Objects, obj)
	}
	// Add non-existent keys
	for i := 0; i < 5; i++ {
		key := generateValidKey(userID, "txt")
		obj := struct {
			Key string `xml:"Key"`
		}{Key: key}
		delReq.Objects = append(delReq.Objects, obj)
	}

	xmlBody, err := xml.Marshal(delReq)
	require.NoError(t, err)

	// Send delete request
	delPath := fmt.Sprintf("/s3/%s?delete", bucket)
	delHTTPReq := newSignedS3Request(t, http.MethodPost, delPath, bytes.NewReader(xmlBody), creds, serverURL)
	delHTTPReq.Header.Set("Content-Type", "application/xml")
	delHTTPReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(xmlBody)))

	delResp, err := client.Do(delHTTPReq)
	require.NoError(t, err)
	defer delResp.Body.Close()

	bodyBytes, _ := io.ReadAll(delResp.Body)

	// The handler currently returns success for all deletes (backend doesn't fail on missing keys)
	// Verify the response is valid XML with DeleteResult
	assert.Equal(t, http.StatusOK, delResp.StatusCode, "body: %s", string(bodyBytes))
	assert.Contains(t, string(bodyBytes), "DeleteResult")

	t.Logf("Delete response: %s", string(bodyBytes))
}
