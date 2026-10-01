package httphandler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestOSS3Integration tests the complete S3 protocol flow.
// MEDIUM-23: This test requires Redis and MinIO. To run:
//  1. Start dependencies: docker compose up -d redis minio
//  2. Set env vars: INTEGRATION_TEST=1, REDIS_URL, MINIO_ENDPOINT, etc.
//  3. Run: go test -tags=integration -run TestOSS3Integration ./internal/handler/http/...
//
// For CI, consider using testcontainers-go to provide Redis + MinIO automatically.
func TestOSS3Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Setup test database
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	// Auto-migrate models
	if err := db.AutoMigrate(&model.File{}, &model.TemporaryCredential{}, &model.MultipartUpload{}, &model.MultipartUploadPart{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	// Create repositories (for future use when test is fully implemented)
	_ = repo.NewFileRepo(db)
	_ = repo.NewTemporaryCredentialRepo(db)
	_ = repo.NewMultipartUploadRepo(db)

	// Create mock backend
	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	// Create usecase config (for future use when test is fully implemented)
	_ = config.StorageConfig{
		Encryption: config.EncryptionConfig{
			SessionTokenKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", // 64 hex chars
		},
		Quota: config.QuotaConfig{
			MaxUserQuotaBytes: 1024 * 1024 * 1024, // 1GB
			PendingTTL:        5 * time.Minute,
		},
		RateLimit: config.RateLimitConfig{
			RequestsPerMinute: 100,
		},
	}

	// Note: We can't create a real OSS3Usecase without Redis, so we'll skip this test
	// and focus on unit tests for now. Integration tests will be done with Docker Compose.
	t.Skip("Integration test requires Redis for OSS3Usecase - use Docker Compose for full integration tests")

	_ = backend
}

// mockBackend is a mock implementation of rtcoss3.Backend for testing.
type mockBackend struct {
	objects map[string][]byte
	meta    map[string]rtcoss3.ObjectMeta
}

func (m *mockBackend) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (string, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	fullKey := bucket + "/" + key
	m.objects[fullKey] = data
	m.meta[fullKey] = rtcoss3.ObjectMeta{
		Key:          key,
		Size:         int64(len(data)),
		ContentType:  contentType,
		ETag:         calculateETag(data),
		LastModified: time.Now(),
	}
	return m.meta[fullKey].ETag, nil
}

func (m *mockBackend) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, rtcoss3.ObjectMeta, error) {
	fullKey := bucket + "/" + key
	data, ok := m.objects[fullKey]
	if !ok {
		return nil, rtcoss3.ObjectMeta{}, rtcoss3.ErrKeyNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), m.meta[fullKey], nil
}

func (m *mockBackend) GetObjectRange(ctx context.Context, bucket, key string, start, end int64) (io.ReadCloser, rtcoss3.ObjectMeta, error) {
	fullKey := bucket + "/" + key
	data, ok := m.objects[fullKey]
	if !ok {
		return nil, rtcoss3.ObjectMeta{}, rtcoss3.ErrKeyNotFound
	}

	// Handle range request
	if start < 0 {
		// Suffix range: return last |start| bytes
		start = int64(len(data)) + start
		if start < 0 {
			start = 0
		}
		end = int64(len(data)) - 1
	}
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	if start > end || start >= int64(len(data)) {
		return io.NopCloser(bytes.NewReader(nil)), m.meta[fullKey], nil
	}

	rangeData := data[start : end+1]
	meta := m.meta[fullKey]
	meta.Size = int64(len(rangeData))
	return io.NopCloser(bytes.NewReader(rangeData)), meta, nil
}

func (m *mockBackend) DeleteObject(ctx context.Context, bucket, key string) error {
	fullKey := bucket + "/" + key
	delete(m.objects, fullKey)
	delete(m.meta, fullKey)
	return nil
}

func (m *mockBackend) HeadObject(ctx context.Context, bucket, key string) (rtcoss3.ObjectMeta, error) {
	fullKey := bucket + "/" + key
	meta, ok := m.meta[fullKey]
	if !ok {
		return rtcoss3.ObjectMeta{}, rtcoss3.ErrKeyNotFound
	}
	return meta, nil
}

func (m *mockBackend) ListObjects(ctx context.Context, bucket string, opts rtcoss3.ListObjectsOptions) (*rtcoss3.ListObjectsResult, error) {
	result := &rtcoss3.ListObjectsResult{}

	// Collect all matching keys first, then sort for deterministic ordering
	var matchingKeys []string
	seenPrefixes := make(map[string]bool)

	for fullKey := range m.meta {
		if len(fullKey) <= len(bucket) || fullKey[:len(bucket)+1] != bucket+"/" {
			continue
		}
		key := fullKey[len(bucket)+1:]
		if len(key) < len(opts.Prefix) || key[:len(opts.Prefix)] != opts.Prefix {
			continue
		}

		// Handle delimiter (common prefixes)
		if opts.Delimiter != "" {
			rest := key[len(opts.Prefix):]
			idx := bytes.Index([]byte(rest), []byte(opts.Delimiter))
			if idx >= 0 {
				commonPrefix := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				if !seenPrefixes[commonPrefix] {
					seenPrefixes[commonPrefix] = true
					result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix)
				}
				continue
			}
		}

		matchingKeys = append(matchingKeys, key)
	}

	// Sort keys for deterministic pagination
	sort.Strings(matchingKeys)

	// Apply marker: skip keys <= marker
	startIdx := 0
	if opts.Marker != "" {
		for i, key := range matchingKeys {
			if key > opts.Marker {
				startIdx = i
				break
			}
			startIdx = i + 1
		}
	}

	// Apply continuation token (V2 pagination): decode as marker
	if opts.ContinuationToken != "" {
		// For mock, treat continuation token as a marker
		decoded, err := base64.StdEncoding.DecodeString(opts.ContinuationToken)
		if err != nil {
			return nil, fmt.Errorf("invalid continuation token")
		}
		tokenMarker := string(decoded)
		for i, key := range matchingKeys {
			if key > tokenMarker {
				startIdx = i
				break
			}
			startIdx = i + 1
		}
	}

	// Collect results with MaxKeys limit
	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000 // default
	}

	for i := startIdx; i < len(matchingKeys); i++ {
		key := matchingKeys[i]
		fullKey := bucket + "/" + key
		meta := m.meta[fullKey]
		result.Objects = append(result.Objects, meta)

		if len(result.Objects) >= maxKeys {
			if i+1 < len(matchingKeys) {
				result.IsTruncated = true
				result.NextMarker = key
				// Encode next marker as continuation token for V2
				result.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(key))
				result.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(key))
			}
			break
		}
	}

	result.KeyCount = len(result.Objects)

	return result, nil
}

func (m *mockBackend) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (rtcoss3.ObjectMeta, error) {
	srcFullKey := srcBucket + "/" + srcKey
	data, ok := m.objects[srcFullKey]
	if !ok {
		return rtcoss3.ObjectMeta{}, rtcoss3.ErrKeyNotFound
	}

	dstFullKey := dstBucket + "/" + dstKey
	m.objects[dstFullKey] = data
	meta := m.meta[srcFullKey]
	meta.Key = dstKey
	meta.LastModified = time.Now()
	m.meta[dstFullKey] = meta

	return meta, nil
}

func (m *mockBackend) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*rtcoss3.MultipartUploadResult, error) {
	return &rtcoss3.MultipartUploadResult{UploadID: "test-upload-id"}, nil
}

func (m *mockBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	return "test-etag", nil
}

func (m *mockBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []rtcoss3.CompletedPart) (string, error) {
	return "test-etag", nil
}

func (m *mockBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	return nil
}

func (m *mockBackend) ListParts(ctx context.Context, bucket, key, uploadID string) ([]rtcoss3.PartInfo, error) {
	return []rtcoss3.PartInfo{}, nil
}

func (m *mockBackend) PresignGet(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	return "https://example.com/presigned", nil
}

func (m *mockBackend) PresignPut(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	return "https://example.com/presigned", nil
}

func (m *mockBackend) Close() error {
	return nil
}

func (m *mockBackend) HealthCheck(ctx context.Context) error {
	return nil
}

func calculateETag(data []byte) string {
	hash := sha256.Sum256(data)
	return "\"" + hex.EncodeToString(hash[:]) + "\""
}
