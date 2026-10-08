package usecase

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// ============================================================
// Mock backend for orphan reconciler tests
// ============================================================

// mockOrphanBackend implements rtcoss3.Backend for testing the orphan reconciler.
// Only the methods used by the reconciler are implemented; others panic.
type mockOrphanBackend struct {
	mu      sync.Mutex
	objects map[string]rtcoss3.ObjectMeta // key -> metadata
	deleted []string                      // keys that were deleted
}

func newMockOrphanBackend() *mockOrphanBackend {
	return &mockOrphanBackend{
		objects: make(map[string]rtcoss3.ObjectMeta),
	}
}

func (b *mockOrphanBackend) addObject(key string, size int64, lastModified time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = rtcoss3.ObjectMeta{
		Key:          key,
		Size:         size,
		LastModified: lastModified,
	}
}

func (b *mockOrphanBackend) ListObjects(_ context.Context, _ string, opts rtcoss3.ListObjectsOptions) (*rtcoss3.ListObjectsResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	result := &rtcoss3.ListObjectsResult{}

	// Simple pagination: collect all keys, sort, apply marker and maxKeys.
	allKeys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		allKeys = append(allKeys, k)
	}
	// Sort keys lexicographically (simple for tests).
	for i := 0; i < len(allKeys); i++ {
		for j := i + 1; j < len(allKeys); j++ {
			if allKeys[i] > allKeys[j] {
				allKeys[i], allKeys[j] = allKeys[j], allKeys[i]
			}
		}
	}

	// Apply marker (start after).
	startIdx := 0
	if opts.Marker != "" {
		for i, k := range allKeys {
			if k > opts.Marker {
				startIdx = i
				break
			}
		}
	}

	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	endIdx := startIdx + maxKeys
	if endIdx > len(allKeys) {
		endIdx = len(allKeys)
	}

	for _, k := range allKeys[startIdx:endIdx] {
		result.Objects = append(result.Objects, b.objects[k])
	}

	result.IsTruncated = endIdx < len(allKeys)
	if result.IsTruncated && len(result.Objects) > 0 {
		result.NextMarker = result.Objects[len(result.Objects)-1].Key
	}
	result.KeyCount = len(result.Objects)

	return result, nil
}

func (b *mockOrphanBackend) DeleteObjects(_ context.Context, _ string, keys []string) ([]rtcoss3.DeleteResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	results := make([]rtcoss3.DeleteResult, len(keys))
	for i, key := range keys {
		if _, ok := b.objects[key]; ok {
			delete(b.objects, key)
			b.deleted = append(b.deleted, key)
			results[i] = rtcoss3.DeleteResult{Key: key}
		} else {
			results[i] = rtcoss3.DeleteResult{Key: key, Code: "NoSuchKey", Message: "not found"}
		}
	}
	return results, nil
}

func (b *mockOrphanBackend) DeleteObject(_ context.Context, _, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	b.deleted = append(b.deleted, key)
	return nil
}

// Stub implementations for unused interface methods.
func (b *mockOrphanBackend) PutObject(_ context.Context, _, _ string, _ io.Reader, _ int64, _ string) (string, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) GetObject(_ context.Context, _, _ string) (io.ReadCloser, rtcoss3.ObjectMeta, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) GetObjectRange(_ context.Context, _, _ string, _, _ int64) (io.ReadCloser, rtcoss3.ObjectMeta, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) HeadObject(_ context.Context, _, _ string) (rtcoss3.ObjectMeta, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) CopyObject(_ context.Context, _, _, _, _ string) (rtcoss3.ObjectMeta, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) CreateMultipartUpload(_ context.Context, _, _, _ string) (*rtcoss3.MultipartUploadResult, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) UploadPart(_ context.Context, _, _, _ string, _ int, _ io.Reader, _ int64) (string, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) CompleteMultipartUpload(_ context.Context, _, _, _ string, _ []rtcoss3.CompletedPart) (string, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) AbortMultipartUpload(_ context.Context, _, _, _ string) error {
	panic("not implemented")
}
func (b *mockOrphanBackend) ListParts(_ context.Context, _, _, _ string) ([]rtcoss3.PartInfo, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) PresignGet(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) PresignPut(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	panic("not implemented")
}
func (b *mockOrphanBackend) Close() error                        { return nil }
func (b *mockOrphanBackend) HealthCheck(_ context.Context) error { return nil }

// ============================================================
// Mock fileRepo for orphan reconciler tests
// ============================================================

// mockOrphanFileRepo implements only the KeysExist method for orphan tests.
type mockOrphanFileRepo struct {
	existingKeys map[string]bool
}

func (r *mockOrphanFileRepo) KeysExist(_ context.Context, keys []string) (map[string]bool, error) {
	result := make(map[string]bool)
	for _, k := range keys {
		if r.existingKeys[k] {
			result[k] = true
		}
	}
	return result, nil
}

// Stub implementations for unused interface methods.
func (r *mockOrphanFileRepo) Create(_ context.Context, _ *model.File) error {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) GetByUserAndKey(_ context.Context, _, _ string) (*model.File, error) {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) Update(_ context.Context, _ *model.File) error {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) Delete(_ context.Context, _ uuid.UUID) error {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) DeleteByUserAndKey(_ context.Context, _, _ string) error {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) SumSizeByUser(_ context.Context, _ string) (int64, error) {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) SumSizeByUserGrouped(_ context.Context) (map[string]int64, error) {
	panic("not implemented")
}
func (r *mockOrphanFileRepo) SumSizeByUserGroupedPaginated(_ context.Context, _ string, _ int) (map[string]int64, string, error) {
	panic("not implemented")
}

// ============================================================
// Compile-time interface checks
// ============================================================

var _ rtcoss3.Backend = (*mockOrphanBackend)(nil)
var _ repo.FileRepo = (*mockOrphanFileRepo)(nil)

// ============================================================
// Tests
// ============================================================

// TestReconcileOrphans_Disabled verifies that the reconciler is a no-op when disabled.
func TestReconcileOrphans_Disabled(t *testing.T) {
	uc := &OSS3Usecase{
		cfg: config.StorageConfig{
			Cleanup: config.CleanupConfig{
				Orphan: config.OrphanCleanupConfig{
					Enabled: false,
				},
			},
		},
	}

	result, err := uc.ReconcileOrphans(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ObjectsScanned != 0 {
		t.Errorf("expected 0 objects scanned, got %d", result.ObjectsScanned)
	}
}

// TestReconcileOrphans_LockFailure verifies that when Redis is unavailable,
// the reconciler returns an error from lock acquisition.
func TestReconcileOrphans_LockFailure(t *testing.T) {
	backend := newMockOrphanBackend()
	backend.addObject("user-1/file.txt", 100, time.Now().Add(-2*time.Hour))

	fileRepo := &mockOrphanFileRepo{existingKeys: map[string]bool{}}
	uc := newOrphanTestUsecase(backend, fileRepo, true)

	_, err := uc.ReconcileOrphans(context.Background())
	if err == nil {
		t.Fatal("expected error from lock acquisition without Redis, got nil")
	}
}

// TestDeleteOrphanBatch_DeletesSuccessfully verifies batch deletion logic.
func TestDeleteOrphanBatch_DeletesSuccessfully(t *testing.T) {
	backend := newMockOrphanBackend()
	backend.addObject("orphan1.txt", 100, time.Now())
	backend.addObject("orphan2.txt", 200, time.Now())

	uc := &OSS3Usecase{
		backend: backend,
	}

	keyMeta := map[string]rtcoss3.ObjectMeta{
		"orphan1.txt": {Key: "orphan1.txt", Size: 100},
		"orphan2.txt": {Key: "orphan2.txt", Size: 200},
	}

	deleted, bytesFreed, err := uc.deleteOrphanBatch(context.Background(), "test-bucket",
		[]string{"orphan1.txt", "orphan2.txt"}, keyMeta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}
	if bytesFreed != 300 {
		t.Errorf("expected 300 bytes freed, got %d", bytesFreed)
	}
}

// TestDeleteOrphanBatch_PartialFailure verifies that partial deletion failures
// are handled gracefully.
func TestDeleteOrphanBatch_PartialFailure(t *testing.T) {
	backend := newMockOrphanBackend()
	backend.addObject("orphan1.txt", 100, time.Now())
	// "orphan2.txt" is NOT in backend — will fail to delete

	uc := &OSS3Usecase{
		backend: backend,
	}

	keyMeta := map[string]rtcoss3.ObjectMeta{
		"orphan1.txt": {Key: "orphan1.txt", Size: 100},
		"orphan2.txt": {Key: "orphan2.txt", Size: 200},
	}

	deleted, bytesFreed, err := uc.deleteOrphanBatch(context.Background(), "test-bucket",
		[]string{"orphan1.txt", "orphan2.txt"}, keyMeta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted (partial), got %d", deleted)
	}
	if bytesFreed != 100 {
		t.Errorf("expected 100 bytes freed, got %d", bytesFreed)
	}
}

// TestOrphanReconcileResult_Fields verifies the result struct has expected fields.
func TestOrphanReconcileResult_Fields(t *testing.T) {
	r := OrphanReconcileResult{
		ObjectsScanned: 100,
		OrphansFound:   5,
		OrphansDeleted: 3,
		OrphansSkipped: 2,
		BytesFreed:     1024,
		Duration:       time.Second,
	}
	if r.ObjectsScanned != 100 {
		t.Errorf("ObjectsScanned: got %d, want 100", r.ObjectsScanned)
	}
	if r.OrphansDeleted != 3 {
		t.Errorf("OrphansDeleted: got %d, want 3", r.OrphansDeleted)
	}
}

// ============================================================
// Test helpers
// ============================================================

// newOrphanTestUsecase creates an OSS3Usecase configured for orphan reconciler testing.
// Note: Without Redis, lock acquisition will fail. Tests verify this behavior.
func newOrphanTestUsecase(backend *mockOrphanBackend, fileRepo *mockOrphanFileRepo, dryRun bool) *OSS3Usecase {
	return &OSS3Usecase{
		backend:  backend,
		fileRepo: fileRepo,
		cfg: config.StorageConfig{
			MinIO: config.MinIOConfig{
				Bucket: "test-bucket",
			},
			Cleanup: config.CleanupConfig{
				Orphan: config.OrphanCleanupConfig{
					Enabled:        true,
					BatchSize:      100,
					CooldownPeriod: 1 * time.Hour,
					DryRun:         dryRun,
				},
			},
		},
		// Redis and scripts are nil — lock acquisition will fail.
	}
}
