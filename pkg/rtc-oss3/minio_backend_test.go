package rtcoss3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMinIOBackendIntegration tests MinIO backend with real MinIO server.
// Requires MinIO running at localhost:29000 with default credentials.
// Run with: go test -tags=integration ./pkg/rtc-oss3/... -v
func TestMinIOBackendIntegration(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") != "1" {
		t.Skip("Set INTEGRATION_TEST=1 to run integration tests")
	}

	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:29000"
	}
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	bucket := "test-rtc-oss3"

	backend, err := NewMinIOBackend(MinIOOptions{
		Endpoint:            endpoint,
		AccessKey:           accessKey,
		SecretKey:           secretKey,
		Bucket:              bucket,
		PublicURL:           "",
		UseSSL:              false,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewMinIOBackend: %v", err)
	}
	defer backend.Close()

	ctx := context.Background()

	t.Run("HealthCheck", func(t *testing.T) {
		if err := backend.HealthCheck(ctx); err != nil {
			t.Errorf("HealthCheck: %v", err)
		}
	})

	t.Run("PutObject", func(t *testing.T) {
		data := []byte("hello world")
		etag, err := backend.PutObject(ctx, bucket, "test/hello.txt", bytes.NewReader(data), int64(len(data)), "text/plain")
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		if etag == "" {
			t.Error("PutObject: expected non-empty ETag")
		}
	})

	t.Run("GetObject", func(t *testing.T) {
		reader, meta, err := backend.GetObject(ctx, bucket, "test/hello.txt")
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		defer reader.Close()

		if meta.Key != "test/hello.txt" {
			t.Errorf("GetObject: expected key 'test/hello.txt', got %q", meta.Key)
		}
		if meta.Size != 11 {
			t.Errorf("GetObject: expected size 11, got %d", meta.Size)
		}

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("GetObject: read body: %v", err)
		}
		if string(data) != "hello world" {
			t.Errorf("GetObject: expected 'hello world', got %q", string(data))
		}
	})

	t.Run("HeadObject", func(t *testing.T) {
		meta, err := backend.HeadObject(ctx, bucket, "test/hello.txt")
		if err != nil {
			t.Fatalf("HeadObject: %v", err)
		}
		if meta.Size != 11 {
			t.Errorf("HeadObject: expected size 11, got %d", meta.Size)
		}
	})

	t.Run("ListObjects", func(t *testing.T) {
		result, err := backend.ListObjects(ctx, bucket, ListObjectsOptions{
			Prefix: "test/",
		})
		if err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
		if len(result.Objects) == 0 {
			t.Error("ListObjects: expected at least one object")
		}
	})

	t.Run("CopyObject", func(t *testing.T) {
		meta, err := backend.CopyObject(ctx, bucket, "test/hello.txt", bucket, "test/hello-copy.txt")
		if err != nil {
			t.Fatalf("CopyObject: %v", err)
		}
		if meta.Key != "test/hello-copy.txt" {
			t.Errorf("CopyObject: expected key 'test/hello-copy.txt', got %q", meta.Key)
		}
	})

	t.Run("DeleteObject", func(t *testing.T) {
		if err := backend.DeleteObject(ctx, bucket, "test/hello.txt"); err != nil {
			t.Fatalf("DeleteObject: %v", err)
		}
		if err := backend.DeleteObject(ctx, bucket, "test/hello-copy.txt"); err != nil {
			t.Fatalf("DeleteObject: %v", err)
		}
	})

	t.Run("MultipartUpload", func(t *testing.T) {
		key := "test/multipart.bin"

		// Create multipart upload
		result, err := backend.CreateMultipartUpload(ctx, bucket, key, "application/octet-stream")
		if err != nil {
			t.Fatalf("CreateMultipartUpload: %v", err)
		}
		uploadID := result.UploadID

		// Upload parts
		part1Data := bytes.Repeat([]byte("a"), 5*1024*1024) // 5MB
		part1ETag, err := backend.UploadPart(ctx, bucket, key, uploadID, 1, bytes.NewReader(part1Data), int64(len(part1Data)))
		if err != nil {
			t.Fatalf("UploadPart 1: %v", err)
		}

		part2Data := bytes.Repeat([]byte("b"), 5*1024*1024) // 5MB
		part2ETag, err := backend.UploadPart(ctx, bucket, key, uploadID, 2, bytes.NewReader(part2Data), int64(len(part2Data)))
		if err != nil {
			t.Fatalf("UploadPart 2: %v", err)
		}

		// List parts
		parts, err := backend.ListParts(ctx, bucket, key, uploadID)
		if err != nil {
			t.Fatalf("ListParts: %v", err)
		}
		if len(parts) != 2 {
			t.Errorf("ListParts: expected 2 parts, got %d", len(parts))
		}

		// Complete multipart upload
		completedParts := []CompletedPart{
			{PartNumber: 1, ETag: part1ETag},
			{PartNumber: 2, ETag: part2ETag},
		}
		etag, err := backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, completedParts)
		if err != nil {
			t.Fatalf("CompleteMultipartUpload: %v", err)
		}
		if etag == "" {
			t.Error("CompleteMultipartUpload: expected non-empty ETag")
		}

		// Verify object exists
		meta, err := backend.HeadObject(ctx, bucket, key)
		if err != nil {
			t.Fatalf("HeadObject after complete: %v", err)
		}
		if meta.Size != 10*1024*1024 {
			t.Errorf("HeadObject: expected size 10MB, got %d", meta.Size)
		}

		// Cleanup
		if err := backend.DeleteObject(ctx, bucket, key); err != nil {
			t.Errorf("DeleteObject: %v", err)
		}
	})

	t.Run("AbortMultipartUpload", func(t *testing.T) {
		key := "test/abort.bin"

		result, err := backend.CreateMultipartUpload(ctx, bucket, key, "application/octet-stream")
		if err != nil {
			t.Fatalf("CreateMultipartUpload: %v", err)
		}

		if err := backend.AbortMultipartUpload(ctx, bucket, key, result.UploadID); err != nil {
			t.Fatalf("AbortMultipartUpload: %v", err)
		}
	})

	t.Run("DeleteObjects", func(t *testing.T) {
		// Create test objects
		for i := 0; i < 3; i++ {
			data := []byte("test data")
			_, _ = backend.PutObject(ctx, bucket, "test/batch-"+string(rune('0'+i))+".txt", bytes.NewReader(data), int64(len(data)), "text/plain")
		}

		// Batch delete
		keys := []string{"test/batch-0.txt", "test/batch-1.txt", "test/batch-2.txt"}
		results, err := backend.DeleteObjects(ctx, bucket, keys)
		if err != nil {
			t.Fatalf("DeleteObjects: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("DeleteObjects: expected 0 errors, got %d", len(results))
		}
	})

	t.Run("ErrorMapping", func(t *testing.T) {
		// Test non-existent object
		_, _, err := backend.GetObject(ctx, bucket, "nonexistent.txt")
		if err == nil {
			t.Error("GetObject: expected error for non-existent object")
		}
		if !strings.Contains(err.Error(), "minio get_object") {
			t.Errorf("GetObject: expected minio error, got: %v", err)
		}
	})
}

// TestMinIOBackendUnit tests MinIO backend error mapping without real MinIO.
func TestMinIOBackendUnit(t *testing.T) {
	t.Run("mapMinIOError_Nil", func(t *testing.T) {
		err := mapMinIOError(context.Background(), nil, "test")
		if err != nil {
			t.Errorf("mapMinIOError(nil): expected nil, got %v", err)
		}
	})

	t.Run("ErrInsufficientStorage_Detection", func(t *testing.T) {
		// Verify disk-full errors map to ErrInsufficientStorage
		diskFullErr := fmt.Errorf("write: no space left on device")
		mapped := mapMinIOError(context.Background(), diskFullErr, "put_object")
		if !errors.Is(mapped, ErrInsufficientStorage) {
			t.Errorf("expected ErrInsufficientStorage, got %v", mapped)
		}

		// Verify nil is not disk full
		if errors.Is(nil, ErrInsufficientStorage) {
			t.Error("nil should not match ErrInsufficientStorage")
		}
	})
}

// makeItems builds a sorted []rawListItem from keys. Items ending with
// "/" are treated as common prefixes (Size=0, no ETag).
func makeItems(keys ...string) []rawListItem {
	items := make([]rawListItem, len(keys))
	for i, k := range keys {
		items[i] = rawListItem{Key: k, Size: 100, ETag: "etag"}
	}
	return items
}

// TestComputeListPagination_NextMarkerTruncatedObjects verifies that when
// truncation happens at a regular object boundary, NextMarker points to
// the last returned object key (not the item that triggered truncation).
func TestComputeListPagination_NextMarkerTruncatedObjects(t *testing.T) {
	// Items: a/1, b, c/1, d, e/1 — sorted lexicographically.
	// With delimiter="/" and maxKeys=3, the first 3 items are returned:
	//   objects=[b], commonPrefixes=[a/, c/], truncated=true
	// NextMarker must be "c/" (last returned item), NOT "d" (the trigger).
	items := makeItems("a/", "b", "c/", "d", "e/")
	objs, prefixes, truncated, marker := computeListPagination(items, "/", 3)

	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(objs) != 1 || objs[0].Key != "b" {
		t.Errorf("objects: got %v, want [b]", objs)
	}
	if len(prefixes) != 2 || prefixes[0] != "a/" || prefixes[1] != "c/" {
		t.Errorf("commonPrefixes: got %v, want [a/ c/]", prefixes)
	}
	if marker != "c/" {
		t.Errorf("NextMarker: got %q, want %q (last returned item, not trigger)", marker, "c/")
	}
}

// TestComputeListPagination_NextMarkerTruncatedCommonPrefixes verifies that
// when all items are CommonPrefixes and truncation occurs, NextMarker
// points to the last returned CommonPrefix.
func TestComputeListPagination_NextMarkerTruncatedCommonPrefixes(t *testing.T) {
	// 5 common prefixes, maxKeys=3 → return first 3, truncated.
	items := makeItems("a/", "b/", "c/", "d/", "e/")
	objs, prefixes, truncated, marker := computeListPagination(items, "/", 3)

	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(objs) != 0 {
		t.Errorf("objects: got %v, want empty", objs)
	}
	if len(prefixes) != 3 {
		t.Fatalf("commonPrefixes: got %v, want 3 items", prefixes)
	}
	if marker != "c/" {
		t.Errorf("NextMarker: got %q, want %q", marker, "c/")
	}
}

// TestComputeListPagination_NotTruncated verifies that when all items fit
// within the quota, IsTruncated is false and NextMarker is empty.
func TestComputeListPagination_NotTruncated(t *testing.T) {
	items := makeItems("a/", "b", "c/")
	objs, prefixes, truncated, marker := computeListPagination(items, "/", 5)

	if truncated {
		t.Error("expected truncated=false")
	}
	if marker != "" {
		t.Errorf("NextMarker: got %q, want empty (not truncated)", marker)
	}
	if len(objs) != 1 || objs[0].Key != "b" {
		t.Errorf("objects: got %v, want [b]", objs)
	}
	if len(prefixes) != 2 {
		t.Errorf("commonPrefixes: got %v, want 2 items", prefixes)
	}
}

// TestComputeListPagination_NoDelimiter verifies that with empty delimiter
// (recursive listing), all items are returned as objects and NextMarker
// uses the last object key.
func TestComputeListPagination_NoDelimiter(t *testing.T) {
	items := makeItems("a", "b", "c", "d")
	objs, prefixes, truncated, marker := computeListPagination(items, "", 3)

	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(prefixes) != 0 {
		t.Errorf("commonPrefixes: got %v, want empty (no delimiter)", prefixes)
	}
	if len(objs) != 3 {
		t.Fatalf("objects: got %d, want 3", len(objs))
	}
	if marker != "c" {
		t.Errorf("NextMarker: got %q, want %q", marker, "c")
	}
}

// TestComputeListPagination_EmptyList verifies handling of empty input.
func TestComputeListPagination_EmptyList(t *testing.T) {
	objs, prefixes, truncated, marker := computeListPagination(nil, "/", 10)

	if truncated {
		t.Error("expected truncated=false for empty list")
	}
	if marker != "" {
		t.Errorf("NextMarker: got %q, want empty", marker)
	}
	if len(objs) != 0 || len(prefixes) != 0 {
		t.Error("expected empty results for empty input")
	}
}

// TestComputeListPagination_ExactlyAtQuota verifies that when items exactly
// fill the quota, IsTruncated is false (we fetch maxKeys+1, so if the
// (maxKeys+1)th doesn't exist, it's not truncated).
func TestComputeListPagination_ExactlyAtQuota(t *testing.T) {
	items := makeItems("a/", "b", "c/")
	objs, prefixes, truncated, marker := computeListPagination(items, "/", 3)

	if truncated {
		t.Error("expected truncated=false when items == maxKeys")
	}
	if marker != "" {
		t.Errorf("NextMarker: got %q, want empty (not truncated)", marker)
	}
	if len(objs)+len(prefixes) != 3 {
		t.Errorf("total items: got %d, want 3", len(objs)+len(prefixes))
	}
}
