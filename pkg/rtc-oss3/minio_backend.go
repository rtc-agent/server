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

// NewMinIOBackend creates a MinIO backend.
// publicURL is the public-facing S3 endpoint for presigned URLs (empty = use endpoint).
func NewMinIOBackend(endpoint, accessKey, secretKey, bucket, publicURL string, useSSL bool, maxIdleConns, maxIdleConnsPerHost int, idleConnTimeout time.Duration) (*MinIOBackend, error) {
	transport := &http.Transport{
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:    useSSL,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("init minio client: %w", err)
	}

	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:    useSSL,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("init minio core: %w", err)
	}

	exists, err := client.BucketExists(context.Background(), bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		logger.Info(context.Background(), "creating MinIO bucket",
			zap.String("bucket", bucket))
		if err := client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}); err != nil {
			// Ignore BucketAlreadyOwnedByYou error (concurrent startup)
			var errResp minio.ErrorResponse
			if errors.As(err, &errResp) && errResp.Code == "BucketAlreadyOwnedByYou" {
				logger.Info(context.Background(), "MinIO bucket already exists (concurrent creation)",
					zap.String("bucket", bucket))
			} else {
				return nil, fmt.Errorf("create bucket: %w", err)
			}
		} else {
			logger.Info(context.Background(), "MinIO bucket created successfully",
				zap.String("bucket", bucket))
		}
	}

	// Create public client for presigned URLs if publicURL is configured
	var publicClient *minio.Client
	if publicURL != "" {
		// Extract host from publicURL (e.g., "http://localhost:29000" -> "localhost:29000")
		publicHost := strings.TrimPrefix(publicURL, "http://")
		publicHost = strings.TrimPrefix(publicHost, "https://")
		publicHost = strings.TrimSuffix(publicHost, "/")

		publicUseSSL := strings.HasPrefix(publicURL, "https://")
		publicClient, err = minio.New(publicHost, &minio.Options{
			Creds:     credentials.NewStaticV4(accessKey, secretKey, ""),
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
		bucket:       bucket,
		publicURL:    publicURL,
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
func (b *MinIOBackend) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]DeleteResult, error) {
	objectsCh := make(chan minio.ObjectInfo, len(keys))
	go func() {
		defer close(objectsCh)
		for _, key := range keys {
			objectsCh <- minio.ObjectInfo{Key: key}
		}
	}()

	results := make([]DeleteResult, 0, len(keys))
	for err := range b.client.RemoveObjects(ctx, bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		results = append(results, DeleteResult{
			Key:     err.ObjectName,
			Code:    "InternalError",
			Message: err.Err.Error(),
		})
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

	// Track common prefixes to avoid duplicates
	commonPrefixes := make(map[string]bool)

	for object := range b.client.ListObjects(ctx, bucket, listOpts) {
		if object.Err != nil {
			return nil, mapMinIOError(object.Err, "list_objects")
		}

		// If delimiter is set, check if this key represents a "directory"
		if opts.Delimiter != "" && !listOpts.Recursive {
			// Extract the part after the prefix
			keyWithoutPrefix := strings.TrimPrefix(object.Key, opts.Prefix)
			// Check if the key contains the delimiter
			if idx := strings.Index(keyWithoutPrefix, opts.Delimiter); idx >= 0 {
				// This is a common prefix (like a directory)
				commonPrefix := opts.Prefix + keyWithoutPrefix[:idx+len(opts.Delimiter)]
				if !commonPrefixes[commonPrefix] {
					commonPrefixes[commonPrefix] = true
					result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix)
				}
				continue
			}
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
func (b *MinIOBackend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	result, err := b.core.ListObjectParts(ctx, bucket, key, uploadID, 0, 10000)
	if err != nil {
		return nil, mapMinIOError(err, "list_parts")
	}
	parts := make([]PartInfo, len(result.ObjectParts))
	for i, p := range result.ObjectParts {
		parts[i] = PartInfo{
			PartNumber:   p.PartNumber,
			Size:         p.Size,
			ETag:         p.ETag,
			LastModified: p.LastModified,
		}
	}
	return parts, nil
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

// IsMinIODiskFullError checks if an error is a disk full condition.
func IsMinIODiskFullError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "no space left on device") ||
		strings.Contains(errStr, "disk full") ||
		strings.Contains(errStr, "disk quota exceeded") ||
		strings.Contains(errStr, "InsufficientStorage")
}
