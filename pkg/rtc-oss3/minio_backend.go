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
		return "", mapMinIOError(err, "put_object")
	}
	return info.ETag, nil
}

// GetObject downloads an object.
func (b *MinIOBackend) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectMeta, error) {
	obj, err := b.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectMeta{}, mapMinIOError(err, "get_object")
	}

	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, ObjectMeta{}, mapMinIOError(err, "get_object.stat")
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
		return nil, ObjectMeta{}, mapMinIOError(err, "get_object_range")
	}

	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, ObjectMeta{}, mapMinIOError(err, "get_object_range.stat")
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
		return mapMinIOError(err, "delete_object")
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
			objectsCh <- minio.ObjectInfo{Key: key}
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
		return ObjectMeta{}, mapMinIOError(err, "head_object")
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
func (b *MinIOBackend) ListObjects(ctx context.Context, bucket string, opts ListObjectsOptions) (*ListObjectsResult, error) {
	result := &ListObjectsResult{}

	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	listOpts := minio.ListObjectsOptions{
		Prefix:    opts.Prefix,
		Recursive: opts.Delimiter == "",
		MaxKeys:   maxKeys,
	}

	// LOW-05 fix: Support pagination parameters
	// Priority: ContinuationToken (V2) > StartAfter (V2) > Marker (V1)
	if opts.ContinuationToken != "" {
		// V2: ContinuationToken is the key to start after (from previous NextContinuationToken)
		listOpts.StartAfter = opts.ContinuationToken
	} else if opts.StartAfter != "" {
		// V2: StartAfter is the key to start after (first request only)
		listOpts.StartAfter = opts.StartAfter
	} else if opts.Marker != "" {
		// V1: Marker is the key to start after
		listOpts.StartAfter = opts.Marker
	}

	for object := range b.client.ListObjects(ctx, bucket, listOpts) {
		if object.Err != nil {
			return nil, mapMinIOError(object.Err, "list_objects")
		}

		// If delimiter is set, keys ending with delimiter are common prefixes
		if opts.Delimiter != "" && strings.HasSuffix(object.Key, opts.Delimiter) {
			result.CommonPrefixes = append(result.CommonPrefixes, object.Key)
			continue
		}

		result.Objects = append(result.Objects, ObjectMeta{
			Key:          object.Key,
			Size:         object.Size,
			ContentType:  object.ContentType,
			ETag:         object.ETag,
			LastModified: object.LastModified,
		})
		if len(result.Objects) >= maxKeys {
			result.IsTruncated = true
			result.NextMarker = object.Key
			result.NextContinuationToken = object.Key
			break
		}
	}

	return result, nil
}

// CopyObject copies an object.
func (b *MinIOBackend) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (ObjectMeta, error) {
	src := minio.CopySrcOptions{Bucket: srcBucket, Object: srcKey}
	dst := minio.CopyDestOptions{Bucket: dstBucket, Object: dstKey}

	info, err := b.client.CopyObject(ctx, dst, src)
	if err != nil {
		return ObjectMeta{}, mapMinIOError(err, "copy_object")
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
		return nil, mapMinIOError(err, "create_multipart_upload")
	}
	return &MultipartUploadResult{UploadID: uploadID}, nil
}

// UploadPart uploads a single part.
func (b *MinIOBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	part, err := b.core.PutObjectPart(ctx, bucket, key, uploadID, partNumber, reader, size, minio.PutObjectPartOptions{})
	if err != nil {
		return "", mapMinIOError(err, "upload_part")
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
		return "", mapMinIOError(err, "complete_multipart_upload")
	}
	return info.ETag, nil
}

// AbortMultipartUpload aborts a multipart upload.
func (b *MinIOBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	err := b.core.AbortMultipartUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return mapMinIOError(err, "abort_multipart_upload")
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
			return nil, mapMinIOError(err, "list_parts")
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
func mapMinIOError(err error, operation string) error {
	if err == nil {
		return nil
	}

	// Try typed error detection first (preferred for MinIO SDK errors)
	var errResp minio.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.Code {
		case "NoSuchKey":
			logger.Debug(context.Background(), "MinIO error mapped: NoSuchKey",
				zap.String("operation", operation))
			return fmt.Errorf("minio %s: %w", operation, ErrBackendKeyNotFound)
		case "AccessDenied":
			logger.Debug(context.Background(), "MinIO error mapped: AccessDenied",
				zap.String("operation", operation))
			return fmt.Errorf("minio %s: %w", operation, ErrBackendAccessDenied)
		case "NoSuchBucket":
			logger.Debug(context.Background(), "MinIO error mapped: NoSuchBucket",
				zap.String("operation", operation))
			return fmt.Errorf("minio %s: %w", operation, ErrBackendBucketNotFound)
		case "InsufficientStorage":
			logger.Debug(context.Background(), "MinIO error mapped: InsufficientStorage",
				zap.String("operation", operation))
			return fmt.Errorf("minio %s: %w", operation, ErrInsufficientStorage)
		}
	}

	// Fallback to string matching for network-level errors
	errStr := err.Error()

	// Disk full / quota — map to ErrInsufficientStorage
	if strings.Contains(errStr, "no space left on device") ||
		strings.Contains(errStr, "disk full") ||
		strings.Contains(errStr, "disk quota exceeded") {
		return fmt.Errorf("minio %s: %w", operation, ErrInsufficientStorage)
	}

	// Key not found — 404-style errors from MinIO.
	if strings.Contains(errStr, "object does not exist") ||
		strings.Contains(errStr, "The specified key does not exist") ||
		strings.Contains(errStr, "resource not found") {
		return fmt.Errorf("minio %s: %w", operation, ErrBackendKeyNotFound)
	}

	// Access denied — permission errors from MinIO.
	if strings.Contains(errStr, "Access Denied") ||
		strings.Contains(errStr, "access denied") {
		return fmt.Errorf("minio %s: %w", operation, ErrBackendAccessDenied)
	}

	// Bucket not found.
	if strings.Contains(errStr, "bucket does not exist") ||
		strings.Contains(errStr, "The specified bucket does not exist") {
		return fmt.Errorf("minio %s: %w", operation, ErrBackendBucketNotFound)
	}

	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no such host") {
		return fmt.Errorf("minio %s: %w", operation, err)
	}

	if strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "context deadline exceeded") {
		return fmt.Errorf("minio %s timeout: %w", operation, err)
	}

	return fmt.Errorf("minio %s: %w", operation, err)
}
