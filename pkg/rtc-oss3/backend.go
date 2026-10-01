package rtcoss3

import (
	"context"
	"io"
	"time"
)

// ObjectMeta holds object metadata returned by HeadObject.
type ObjectMeta struct {
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// MultipartUploadResult holds the result of CreateMultipartUpload.
type MultipartUploadResult struct {
	UploadID string
}

// CompletedPart represents a completed part for CompleteMultipartUpload.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// PartInfo holds information about an uploaded part.
type PartInfo struct {
	PartNumber   int
	Size         int64
	ETag         string
	LastModified time.Time
}

// ListObjectsResult holds the result of ListObjects.
type ListObjectsResult struct {
	Objects               []ObjectMeta
	CommonPrefixes        []string
	NextMarker            string // V1 semantics
	NextContinuationToken string // V2 semantics (R9 H4)
	IsTruncated           bool
}

// ListObjectsOptions holds options for ListObjects.
// Supports both V1 (Marker) and V2 (ContinuationToken, StartAfter) semantics.
type ListObjectsOptions struct {
	Prefix            string
	Delimiter         string
	Marker            string // V1 semantics (backward compatible)
	ContinuationToken string // V2 semantics (R9 H4)
	StartAfter        string // V2 semantics: start key (exclusive), first request only (R9 H4)
	MaxKeys           int    // Default 1000
}

// DeleteResult holds the result of a single DeleteObjects entry.
// One DeleteResult is returned per requested key. A successful deletion has
// Code == "" and Message == ""; a failed deletion has a non-empty Code
// (e.g. "InternalError") and a descriptive Message.
type DeleteResult struct {
	Key          string
	Code         string // empty on success, S3 error code on failure
	Message      string
	DeleteMarker bool // true if a delete marker was created (versioning)
}

// Backend is the storage backend interface.
//
// # Bucket Parameter Design
//
// Most methods accept a `bucket` parameter per call, allowing callers to
// address different buckets on the same backend instance (multi-bucket
// support). The `bucket` field stored on the implementation (set via the
// constructor) is reserved for lifecycle operations — specifically
// HealthCheck — which performs a simple readiness probe against the
// primary bucket the backend was configured with.
//
// Implementations must be safe for concurrent use.
type Backend interface {
	// Basic object operations
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (etag string, err error)
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectMeta, error)
	GetObjectRange(ctx context.Context, bucket, key string, start, end int64) (io.ReadCloser, ObjectMeta, error) // R9 H2: Range download
	DeleteObject(ctx context.Context, bucket, key string) error
	// DeleteObjects performs batch deletion of multiple objects.
	// NOTE: This method is implemented but not yet exposed as an HTTP endpoint
	// (POST /{bucket}?delete=). It is available for internal use and future API exposure.
	DeleteObjects(ctx context.Context, bucket string, keys []string) ([]DeleteResult, error) // R9 H3: Batch delete — returns one result per key; success has Code==""
	HeadObject(ctx context.Context, bucket, key string) (ObjectMeta, error)
	ListObjects(ctx context.Context, bucket string, opts ListObjectsOptions) (*ListObjectsResult, error)
	CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (ObjectMeta, error)

	// Multipart upload operations
	CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*MultipartUploadResult, error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (etag string, err error)
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (etag string, err error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error)

	// Presigned URLs
	PresignGet(ctx context.Context, bucket, key string, expiry time.Duration) (string, error)
	PresignPut(ctx context.Context, bucket, key string, expiry time.Duration) (string, error)

	// Lifecycle

	// Close releases any resources held by the backend (connections, caches,
	// etc.). The MinIO implementation is currently a no-op, but the method
	// exists so future backends (e.g. S3 with persistent HTTP clients) can
	// clean up gracefully without changing the interface.
	Close() error

	// HealthCheck verifies the storage backend is reachable.
	// Used by Readyz endpoint (1G-6) to report storage readiness.
	HealthCheck(ctx context.Context) error
}
