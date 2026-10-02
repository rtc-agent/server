package usecase

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/repo"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// newTestOSS3Usecase creates a minimal OSS3Usecase backed by miniredis and sqlite.
func newTestOSS3Usecase(t *testing.T) (*OSS3Usecase, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	t.Cleanup(mr.Close)

	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	scripts := cache.RegisterOSS3Scripts(rdb)

	uc, err := NewOSS3Usecase(
		&noopBackend{},
		db,
		repo.NewFileRepo(db),
		repo.NewMultipartUploadRepo(db),
		repo.NewTemporaryCredentialRepo(db),
		rdb,
		scripts,
		config.StorageConfig{
			Encryption: config.EncryptionConfig{
				SessionTokenKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
		},
	)
	if err != nil {
		t.Fatalf("new usecase: %v", err)
	}
	return uc, mr
}

// noopBackend is a minimal stub that satisfies rtcoss3.Backend for lock tests.
type noopBackend struct{ rtcoss3.Backend }

func (noopBackend) CreateMultipartUpload(context.Context, string, string, string) (*rtcoss3.MultipartUploadResult, error) {
	return &rtcoss3.MultipartUploadResult{UploadID: "test"}, nil
}
func (noopBackend) UploadPart(context.Context, string, string, string, int, io.Reader, int64) (string, error) {
	return "etag", nil
}
func (noopBackend) CompleteMultipartUpload(context.Context, string, string, string, []rtcoss3.CompletedPart) (string, error) {
	return "etag", nil
}
func (noopBackend) AbortMultipartUpload(context.Context, string, string, string) error {
	return nil
}
func (noopBackend) ListParts(context.Context, string, string, string) ([]rtcoss3.PartInfo, error) {
	return nil, nil
}

// TestUploadPartLock_BasicAcquireRelease verifies acquire -> release -> re-acquire works.
func TestUploadPartLock_BasicAcquireRelease(t *testing.T) {
	uc, _ := newTestOSS3Usecase(t)
	ctx := context.Background()
	uploadID := "upload-basic"

	// First acquire should succeed
	holder1, ok, err := uc.AcquireUploadPartLock(ctx, uploadID, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	if !ok {
		t.Fatal("expected lock acquire to succeed")
	}
	if holder1 == "" {
		t.Fatal("expected non-empty holder UUID")
	}

	// Second acquire by a different caller should fail (lock is held)
	_, ok2, err := uc.AcquireUploadPartLock(ctx, uploadID, 5*time.Second)
	if err != nil {
		t.Fatalf("second acquire lock: %v", err)
	}
	if ok2 {
		t.Fatal("expected second lock acquire to fail while lock is held")
	}

	// Release the lock
	uc.ReleaseUploadPartLock(ctx, uploadID, holder1)

	// Third acquire should succeed after release
	_, ok3, err := uc.AcquireUploadPartLock(ctx, uploadID, 5*time.Second)
	if err != nil {
		t.Fatalf("third acquire lock: %v", err)
	}
	if !ok3 {
		t.Fatal("expected lock acquire to succeed after release")
	}
}

// TestUploadPartLock_DifferentUploadIDs verifies locks for different uploads don't interfere.
func TestUploadPartLock_DifferentUploadIDs(t *testing.T) {
	uc, _ := newTestOSS3Usecase(t)
	ctx := context.Background()

	holder1, ok1, err := uc.AcquireUploadPartLock(ctx, "upload-A", 5*time.Second)
	if err != nil {
		t.Fatalf("acquire lock A: %v", err)
	}
	if !ok1 {
		t.Fatal("expected lock A acquire to succeed")
	}

	// Lock for a different uploadID should succeed independently
	holder2, ok2, err := uc.AcquireUploadPartLock(ctx, "upload-B", 5*time.Second)
	if err != nil {
		t.Fatalf("acquire lock B: %v", err)
	}
	if !ok2 {
		t.Fatal("expected lock B acquire to succeed (different uploadID)")
	}
	if holder1 == holder2 {
		t.Fatal("holder UUIDs should be unique")
	}

	uc.ReleaseUploadPartLock(ctx, "upload-A", holder1)
	uc.ReleaseUploadPartLock(ctx, "upload-B", holder2)
}

// TestUploadPartLock_TTLExpiry verifies the lock auto-expires after TTL.
func TestUploadPartLock_TTLExpiry(t *testing.T) {
	uc, mr := newTestOSS3Usecase(t)
	ctx := context.Background()
	uploadID := "upload-ttl"

	_, ok, err := uc.AcquireUploadPartLock(ctx, uploadID, 1*time.Second)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	if !ok {
		t.Fatal("expected lock acquire to succeed")
	}

	// Fast-forward miniredis time past the TTL
	mr.FastForward(2 * time.Second)

	// After TTL expiry, a new acquire should succeed
	_, ok2, err := uc.AcquireUploadPartLock(ctx, uploadID, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire after TTL: %v", err)
	}
	if !ok2 {
		t.Fatal("expected lock acquire to succeed after TTL expiry")
	}
}

// TestUploadPartLock_ConcurrentAcquire serializes concurrent acquire attempts.
// Only one goroutine should succeed at a time for the same uploadID.
func TestUploadPartLock_ConcurrentAcquire(t *testing.T) {
	uc, _ := newTestOSS3Usecase(t)
	ctx := context.Background()
	uploadID := "upload-concurrent"

	const goroutines = 10
	var wg sync.WaitGroup
	acquired := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			holder, ok, err := uc.AcquireUploadPartLock(ctx, uploadID, 10*time.Second)
			if err != nil {
				return
			}
			if ok {
				acquired <- holder
			}
		}()
	}

	wg.Wait()
	close(acquired)

	// Exactly one goroutine should have acquired the lock
	var holders []string
	for h := range acquired {
		holders = append(holders, h)
	}
	if len(holders) != 1 {
		t.Fatalf("expected exactly 1 lock holder, got %d", len(holders))
	}
}
