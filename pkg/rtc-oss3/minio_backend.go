package rtcoss3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/rtc-agent/server/pkg/logger"
	"go.uber.org/zap"
)

// MinIOBackend uses both minio.Client (basic ops) and minio.Core (multipart ops).
type MinIOBackend struct {
	client       *minio.Client
	core         *minio.Core
	publicClient *minio.Client // Client with public endpoint for presigned URLs (nil if publicURL is empty)
	bucket       string
	publicURL    string // Public-facing URL for presigned URLs (empty = use client endpoint)
}

// MinIOOptions holds configuration for NewMinIOBackend.
type MinIOOptions struct {
	Endpoint            string
	AccessKey           string
	SecretKey           string
	Bucket              string
	PublicURL           string // Public-facing S3 endpoint for presigned URLs (empty = use Endpoint)
	UseSSL              bool
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
}

// NewMinIOBackend creates a MinIO backend.
func NewMinIOBackend(opts MinIOOptions) (*MinIOBackend, error) {
	transport := &http.Transport{
		MaxIdleConns:          opts.MaxIdleConns,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}

	client, err := minio.New(opts.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure:    opts.UseSSL,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("init minio client: %w", err)
	}

	// LOW-06 fix: Set application info for better observability in MinIO logs
	client.SetAppInfo("rtc-agent", "1.0.0")

	core, err := minio.NewCore(opts.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure:    opts.UseSSL,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("init minio core: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exists, err := client.BucketExists(ctx, opts.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		logger.Info(ctx, "creating MinIO bucket",
			zap.String("bucket", opts.Bucket))
		if err := client.MakeBucket(ctx, opts.Bucket, minio.MakeBucketOptions{}); err != nil {
			// Ignore BucketAlreadyOwnedByYou error (concurrent startup)
			var errResp minio.ErrorResponse
			if errors.As(err, &errResp) && errResp.Code == "BucketAlreadyOwnedByYou" {
				logger.Info(ctx, "MinIO bucket already exists (concurrent creation)",
					zap.String("bucket", opts.Bucket))
			} else {
				return nil, fmt.Errorf("create bucket: %w", err)
			}
		} else {
			logger.Info(ctx, "MinIO bucket created successfully",
				zap.String("bucket", opts.Bucket))
		}
	}

	// Create public client for presigned URLs if publicURL is configured
	var publicClient *minio.Client
	if opts.PublicURL != "" {
		// Extract host from publicURL (e.g., "http://localhost:29000" -> "localhost:29000")
		publicHost := strings.TrimPrefix(opts.PublicURL, "http://")
		publicHost = strings.TrimPrefix(publicHost, "https://")
		publicHost = strings.TrimSuffix(publicHost, "/")

		publicUseSSL := strings.HasPrefix(opts.PublicURL, "https://")
		publicClient, err = minio.New(publicHost, &minio.Options{
			Creds:     credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
			Secure:    publicUseSSL,
			Transport: transport,
			Region:    "us-east-1", // Set region to avoid GetBucketLocation call
		})
		if err != nil {
			return nil, fmt.Errorf("init public minio client: %w", err)
		}
	}

	return &MinIOBackend{
		client:       client,
		core:         core,
		publicClient: publicClient,
		bucket:       opts.Bucket,
		publicURL:    opts.PublicURL,
	}, nil
}

// PutObject uploads an object.
func (b *MinIOBackend) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (string, error) {
	info, err := b.client.PutObject(ctx, bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return "", mapMinIOError(ctx, err, "put_object")
	}
	return info.ETag, nil
}

// GetObject downloads an object.
func (b *MinIOBackend) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectMeta, error) {
	obj, err := b.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectMeta{}, mapMinIOError(ctx, err, "get_object")
	}

	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, ObjectMeta{}, mapMinIOError(ctx, err, "get_object.stat")
	}

	meta := ObjectMeta{
		Key:          stat.Key,
		Size:         stat.Size,
		ContentType:  stat.ContentType,
		ETag:         stat.ETag,
		LastModified: stat.LastModified,
	}
	return obj, meta, nil
}

// GetObjectRange downloads a byte range of an object.
func (b *MinIOBackend) GetObjectRange(ctx context.Context, bucket, key string, start, end int64) (io.ReadCloser, ObjectMeta, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(start, end); err != nil {
		return nil, ObjectMeta{}, fmt.Errorf("set range: %w", err)
	}

	obj, err := b.client.GetObject(ctx, bucket, key, opts)
	if err != nil {
		return nil, ObjectMeta{}, mapMinIOError(ctx, err, "get_object_range")
	}

	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, ObjectMeta{}, mapMinIOError(ctx, err, "get_object_range.stat")
	}

	meta := ObjectMeta{
		Key:          stat.Key,
		Size:         stat.Size,
		ContentType:  stat.ContentType,
		ETag:         stat.ETag,
		LastModified: stat.LastModified,
	}
	return obj, meta, nil
}

// DeleteObject deletes an object.
func (b *MinIOBackend) DeleteObject(ctx context.Context, bucket, key string) error {
	err := b.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return mapMinIOError(ctx, err, "delete_object")
	}
	return nil
}

// DeleteObjects batch deletes objects.
// Returns one DeleteResult per input key. A successful deletion has Code=="";
// a failed deletion has Code="InternalError" and a descriptive Message.
func (b *MinIOBackend) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]DeleteResult, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	objectsCh := make(chan minio.ObjectInfo, len(keys))
	go func() {
		defer close(objectsCh)
		for _, key := range keys {
			select {
			case objectsCh <- minio.ObjectInfo{Key: key}:
			case <-ctx.Done():
				return // Exit if context cancelled — prevents goroutine leak
			}
		}
	}()

	// Collect failures keyed by object name.
	failures := make(map[string]string, len(keys))
	for err := range b.client.RemoveObjects(ctx, bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		failures[err.ObjectName] = err.Err.Error()
	}

	// Build a result for every requested key so callers can distinguish
	// "all succeeded" from "no-op".
	results := make([]DeleteResult, len(keys))
	for i, key := range keys {
		if msg, failed := failures[key]; failed {
			results[i] = DeleteResult{
				Key:     key,
				Code:    "InternalError",
				Message: msg,
			}
		} else {
			results[i] = DeleteResult{Key: key} // Code == "" => success
		}
	}
	return results, nil
}

// HeadObject retrieves object metadata.
func (b *MinIOBackend) HeadObject(ctx context.Context, bucket, key string) (ObjectMeta, error) {
	stat, err := b.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectMeta{}, mapMinIOError(ctx, err, "head_object")
	}
	return ObjectMeta{
		Key:          stat.Key,
		Size:         stat.Size,
		ContentType:  stat.ContentType,
		ETag:         stat.ETag,
		LastModified: stat.LastModified,
	}, nil
}

// ListObjects lists objects.
// MEDIUM-22 fix: When delimiter is set and Recursive=false, the MinIO SDK handles
// common prefix extraction server-side. Objects whose keys end with the delimiter
// are common prefixes (directories); others are regular objects.
// LOW-05 fix: Support pagination via Marker, ContinuationToken, and StartAfter.
// Pagination fix: Use MaxKeys+1 strategy to accurately detect truncation.
// CommonPrefixes count towards the MaxKeys quota (totalKeys tracks both Objects and CommonPrefixes).
//
// Boundary condition handling (H2):
//   - We request maxKeys+1 items from MinIO to detect if there are more items
//   - Both Objects and CommonPrefixes count towards maxKeys limit (per S3 spec)
//   - If we receive maxKeys+1 items, we return maxKeys and set IsTruncated=true
//   - If we receive <= maxKeys items, we return all and set IsTruncated=false
//   - Edge case: if the (maxKeys+1)th item is a CommonPrefix, we correctly detect
//     truncation but don't include it in results (client fetches next page)
func (b *MinIOBackend) ListObjects(ctx context.Context, bucket string, opts ListObjectsOptions) (*ListObjectsResult, error) {
	result := &ListObjectsResult{}

	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	listOpts := minio.ListObjectsOptions{
		Prefix:    opts.Prefix,
		Recursive: opts.Delimiter == "",
		MaxKeys:   maxKeys + 1, // fetch one extra to detect truncation accurately
	}

	// Determine pagination start point
	// Priority: ContinuationToken (V2) > StartAfter (V2) > Marker (V1)
	if opts.ContinuationToken != "" {
		// V2: ContinuationToken is opaque (base64-encoded), decode it first
		decoded, err := DecodeContinuationToken(opts.ContinuationToken)
		if err != nil {
			return nil, fmt.Errorf("invalid continuation token: %w", err)
		}
		listOpts.StartAfter = decoded
	} else if opts.StartAfter != "" {
		// V2: StartAfter is the key to start after (first request only)
		listOpts.StartAfter = opts.StartAfter
	} else if opts.Marker != "" {
		// V1: Marker is the key to start after
		listOpts.StartAfter = opts.Marker
	}

	totalKeys := 0
	var lastKey string
	truncated := false

	for object := range b.client.ListObjects(ctx, bucket, listOpts) {
		if object.Err != nil {
			return nil, mapMinIOError(ctx, object.Err, "list_objects")
		}

		lastKey = object.Key

		// If delimiter is set, keys ending with delimiter are common prefixes
		if opts.Delimiter != "" && strings.HasSuffix(object.Key, opts.Delimiter) {
			// CommonPrefixes count towards MaxKeys quota
			totalKeys++
			if totalKeys > maxKeys {
				truncated = true
				break
			}
			result.CommonPrefixes = append(result.CommonPrefixes, object.Key)
			continue
		}

		// Regular object - check if we've reached the quota
		if totalKeys >= maxKeys {
			truncated = true
			break
		}

		result.Objects = append(result.Objects, ObjectMeta{
			Key:          object.Key,
			Size:         object.Size,
			ContentType:  object.ContentType,
			ETag:         object.ETag,
			LastModified: object.LastModified,
		})
		totalKeys++
	}

	result.IsTruncated = truncated
	if truncated && lastKey != "" {
		result.NextMarker = lastKey
		result.NextContinuationToken = EncodeContinuationToken(lastKey)
	}
	result.KeyCount = len(result.Objects) + len(result.CommonPrefixes)

	return result, nil
}

// CopyObject copies an object.
func (b *MinIOBackend) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (ObjectMeta, error) {
	src := minio.CopySrcOptions{Bucket: srcBucket, Object: srcKey}
	dst := minio.CopyDestOptions{Bucket: dstBucket, Object: dstKey}

	info, err := b.client.CopyObject(ctx, dst, src)
	if err != nil {
		return ObjectMeta{}, mapMinIOError(ctx, err, "copy_object")
	}

	return ObjectMeta{
		Key:          info.Key,
		ETag:         info.ETag,
		Size:         info.Size,
		LastModified: info.LastModified,
	}, nil
}

// CreateMultipartUpload initiates a multipart upload.
func (b *MinIOBackend) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*MultipartUploadResult, error) {
	uploadID, err := b.core.NewMultipartUpload(ctx, bucket, key, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return nil, mapMinIOError(ctx, err, "create_multipart_upload")
	}
	return &MultipartUploadResult{UploadID: uploadID}, nil
}

// UploadPart uploads a single part.
func (b *MinIOBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	part, err := b.core.PutObjectPart(ctx, bucket, key, uploadID, partNumber, reader, size, minio.PutObjectPartOptions{})
	if err != nil {
		return "", mapMinIOError(ctx, err, "upload_part")
	}
	return part.ETag, nil
}

// CompleteMultipartUpload finalizes a multipart upload.
func (b *MinIOBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (string, error) {
	coreParts := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		coreParts[i] = minio.CompletePart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		}
	}
	info, err := b.core.CompleteMultipartUpload(ctx, bucket, key, uploadID, coreParts, minio.PutObjectOptions{})
	if err != nil {
		return "", mapMinIOError(ctx, err, "complete_multipart_upload")
	}
	return info.ETag, nil
}

// AbortMultipartUpload aborts a multipart upload.
func (b *MinIOBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	err := b.core.AbortMultipartUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return mapMinIOError(ctx, err, "abort_multipart_upload")
	}
	return nil
}

// ListParts lists uploaded parts.
// Paginates through all parts using partNumberMarker until IsTruncated is false.
func (b *MinIOBackend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	const maxPartsPerPage = 1000
	var allParts []PartInfo
	partNumberMarker := 0

	for {
		result, err := b.core.ListObjectParts(ctx, bucket, key, uploadID, partNumberMarker, maxPartsPerPage)
		if err != nil {
			return nil, mapMinIOError(ctx, err, "list_parts")
		}

		for _, p := range result.ObjectParts {
			allParts = append(allParts, PartInfo{
				PartNumber:   p.PartNumber,
				Size:         p.Size,
				ETag:         p.ETag,
				LastModified: p.LastModified,
			})
		}

		if !result.IsTruncated {
			break
		}
		// NextPartNumberMarker points to the last part returned; the next
		// call should start *after* it.
		partNumberMarker = result.NextPartNumberMarker
	}

	return allParts, nil
}

// PresignGet generates a presigned GET URL.
// If publicClient is configured, uses it to generate URL with correct public endpoint.
func (b *MinIOBackend) PresignGet(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	client := b.client
	if b.publicClient != nil {
		client = b.publicClient
	}
	u, err := client.PresignedGetObject(ctx, bucket, key, expiry, nil)
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return u.String(), nil
}

// PresignPut generates a presigned PUT URL.
// If publicClient is configured, uses it to generate URL with correct public endpoint.
func (b *MinIOBackend) PresignPut(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	client := b.client
	if b.publicClient != nil {
		client = b.publicClient
	}
	u, err := client.PresignedPutObject(ctx, bucket, key, expiry)
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return u.String(), nil
}

// Close closes the backend (no-op for MinIO).
func (b *MinIOBackend) Close() error {
	return nil
}

// HealthCheck verifies MinIO is reachable.
func (b *MinIOBackend) HealthCheck(ctx context.Context) error {
	exists, err := b.client.BucketExists(ctx, b.bucket)
	if err != nil {
		logger.Warn(ctx, "MinIO health check failed",
			zap.String("bucket", b.bucket),
			zap.Error(err))
		return fmt.Errorf("minio health check: %w", err)
	}
	if !exists {
		logger.Warn(ctx, "MinIO health check: bucket does not exist",
			zap.String("bucket", b.bucket))
		return fmt.Errorf("minio health check: bucket %q does not exist", b.bucket)
	}
	return nil
}

// mapMinIOError translates MinIO SDK errors into backend sentinel errors.
//
// The returned errors are wrapped with the operation name and the underlying
// cause, so callers can detect them with errors.Is(). The handler layer is
// responsible for mapping these sentinels to S3-compatible error responses.
//
// ctx is used for debug logging so trace context propagates correctly; if ctx
// is nil, context.Background() is used as a fallback.
//
// M1: Error detection strategy:
//  1. Typed error detection (errors.As) — preferred, stable across SDK versions
//  2. String matching fallback — may break on SDK upgrades; patterns below are
//     based on minio-go v7.0.x (2026-09). If SDK is upgraded, verify these patterns
//     still match the new error messages.
func mapMinIOError(ctx context.Context, err error, operation string) error {
	if err == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Try typed error detection first (preferred for MinIO SDK errors)
	var errResp minio.ErrorResponse
	if errors.As(err, &errResp) {
		if sentinel, ok := mapTypedMinIOError(ctx, errResp.Code, operation); ok {
			return sentinel
		}
	}

	// Fallback to string matching for network-level errors
	return mapStringMinIOError(err, operation)
}

// mapTypedMinIOError maps MinIO error response codes to sentinel errors.
func mapTypedMinIOError(ctx context.Context, code, operation string) (error, bool) {
	var sentinel error
	switch code {
	case "NoSuchKey":
		sentinel = ErrBackendKeyNotFound
	case "AccessDenied":
		sentinel = ErrBackendAccessDenied
	case "NoSuchBucket":
		sentinel = ErrBackendBucketNotFound
	case "InsufficientStorage":
		sentinel = ErrInsufficientStorage
	default:
		return nil, false
	}
	logger.Debug(ctx, "MinIO error mapped",
		zap.String("operation", operation),
		zap.String("code", code))
	return fmt.Errorf("minio %s: %w", operation, sentinel), true
}

// mapStringMinIOError maps MinIO errors by string matching on the error message.
func mapStringMinIOError(err error, operation string) error {
	errStr := err.Error()

	diskFullPatterns := []string{"no space left on device", "disk full", "disk quota exceeded"}
	if containsAny(errStr, diskFullPatterns) {
		return fmt.Errorf("minio %s: %w", operation, ErrInsufficientStorage)
	}

	keyNotFoundPatterns := []string{"object does not exist", "The specified key does not exist", "resource not found"}
	if containsAny(errStr, keyNotFoundPatterns) {
		return fmt.Errorf("minio %s: %w", operation, ErrBackendKeyNotFound)
	}

	accessDeniedPatterns := []string{"Access Denied", "access denied"}
	if containsAny(errStr, accessDeniedPatterns) {
		return fmt.Errorf("minio %s: %w", operation, ErrBackendAccessDenied)
	}

	bucketNotFoundPatterns := []string{"bucket does not exist", "The specified bucket does not exist"}
	if containsAny(errStr, bucketNotFoundPatterns) {
		return fmt.Errorf("minio %s: %w", operation, ErrBackendBucketNotFound)
	}

	networkPatterns := []string{"connection refused", "no such host"}
	if containsAny(errStr, networkPatterns) {
		return fmt.Errorf("minio %s: %w", operation, err)
	}

	timeoutPatterns := []string{"i/o timeout", "context deadline exceeded"}
	if containsAny(errStr, timeoutPatterns) {
		return fmt.Errorf("minio %s timeout: %w", operation, err)
	}

	return fmt.Errorf("minio %s: %w", operation, err)
}

// containsAny returns true if s contains any of the given substrings.
func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
