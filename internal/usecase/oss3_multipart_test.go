package usecase

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// ============================================================
// Transaction tests for completeMultipartUploadDBOps
// ============================================================

var txTestDBCounter atomic.Int64

// setupTestDBForTx creates an in-memory SQLite DB with all required models migrated.
// Each call uses a unique DSN to isolate parallel tests.
func setupTestDBForTx(t *testing.T) *gorm.DB {
	t.Helper()
	n := txTestDBCounter.Add(1)
	dsn := fmt.Sprintf("file:txtest%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.File{}, &model.MultipartUpload{}, &model.MultipartUploadPart{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

// newTxTestUsecase creates an OSS3Usecase backed by an in-memory SQLite DB.
func newTxTestUsecase(t *testing.T) (*OSS3Usecase, *gorm.DB) {
	t.Helper()
	db := setupTestDBForTx(t)
	uc := &OSS3Usecase{
		db:         db,
		fileRepo:   repo.NewFileRepo(db),
		uploadRepo: repo.NewMultipartUploadRepo(db),
	}
	return uc, db
}

// TestCompleteMultipartUploadDBOps_AllSuccess verifies that when all DB operations
// succeed, the file record is created and the upload record (with parts) is deleted atomically.
func TestCompleteMultipartUploadDBOps_AllSuccess(t *testing.T) {
	uc, db := newTxTestUsecase(t)
	ctx := context.Background()

	// Arrange: create a multipart upload record with parts
	upload := &model.MultipartUpload{
		UserID:   "user-test-1",
		Bucket:   "test-bucket",
		Key:      "user-test-1/file.txt",
		UploadID: "test-upload-id-1",
		Status:   "uploading",
	}
	if err := db.Create(upload).Error; err != nil {
		t.Fatalf("create upload: %v", err)
	}
	part := &model.MultipartUploadPart{
		UploadID:   upload.ID,
		PartNumber: 1,
		Size:       1024,
		ETag:       "etag-part-1",
	}
	if err := db.Create(part).Error; err != nil {
		t.Fatalf("create part: %v", err)
	}

	// Act: run the transactional DB operations
	err := uc.completeMultipartUploadDBOps(ctx, "user-test-1", "test-bucket", "user-test-1/file.txt", "test-upload-id-1", "test-etag", 2048)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert: file record was created
	var fileCount int64
	db.Model(&model.File{}).Where("user_id = ? AND key = ?", "user-test-1", "user-test-1/file.txt").Count(&fileCount)
	if fileCount != 1 {
		t.Errorf("expected 1 file record, got %d", fileCount)
	}

	// Verify file fields
	var file model.File
	db.Where("user_id = ? AND key = ?", "user-test-1", "user-test-1/file.txt").First(&file)
	if file.Size != 2048 {
		t.Errorf("file size: got %d, want 2048", file.Size)
	}
	if file.ETag != "test-etag" {
		t.Errorf("file etag: got %q, want %q", file.ETag, "test-etag")
	}

	// Assert: upload record was deleted
	var uploadCount int64
	db.Model(&model.MultipartUpload{}).Where("upload_id = ?", "test-upload-id-1").Count(&uploadCount)
	if uploadCount != 0 {
		t.Errorf("expected 0 upload records, got %d", uploadCount)
	}

	// Assert: parts were deleted
	var partCount int64
	db.Model(&model.MultipartUploadPart{}).Where("upload_id = ?", upload.ID).Count(&partCount)
	if partCount != 0 {
		t.Errorf("expected 0 part records, got %d", partCount)
	}
}

// TestCompleteMultipartUploadDBOps_RollbackOnFileCreateFailure verifies that when
// the file creation fails, all changes are rolled back and no orphaned records remain.
func TestCompleteMultipartUploadDBOps_RollbackOnFileCreateFailure(t *testing.T) {
	uc, db := newTxTestUsecase(t)
	ctx := context.Background()

	// Arrange: create a multipart upload record
	upload := &model.MultipartUpload{
		UserID:   "user-test-2",
		Bucket:   "test-bucket",
		Key:      "user-test-2/file.txt",
		UploadID: "test-upload-id-2",
		Status:   "uploading",
	}
	if err := db.Create(upload).Error; err != nil {
		t.Fatalf("create upload: %v", err)
	}

	// Wrap fileRepo to force Create to fail
	uc.fileRepo = &failingFileRepo{inner: uc.fileRepo}

	// Act
	err := uc.completeMultipartUploadDBOps(ctx, "user-test-2", "test-bucket", "user-test-2/file.txt", "test-upload-id-2", "new-etag", 2048)

	// Assert: should have failed
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Assert: upload record should still exist (rolled back)
	var uploadCount int64
	db.Model(&model.MultipartUpload{}).Where("upload_id = ?", "test-upload-id-2").Count(&uploadCount)
	if uploadCount != 1 {
		t.Errorf("expected 1 upload record (rollback), got %d", uploadCount)
	}
}

// TestCompleteMultipartUploadDBOps_UploadNotFound verifies that when the upload
// does not exist, the file creation still succeeds (upload deletion is a no-op).
// This matches the idempotent behavior of deleteUploadInTx.
func TestCompleteMultipartUploadDBOps_UploadNotFound(t *testing.T) {
	uc, db := newTxTestUsecase(t)
	ctx := context.Background()

	// Act: call with a non-existent upload ID
	err := uc.completeMultipartUploadDBOps(ctx, "user-test-3", "test-bucket", "user-test-3/file.txt", "nonexistent-upload-id", "test-etag", 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert: file record was created
	var fileCount int64
	db.Model(&model.File{}).Where("key = ?", "user-test-3/file.txt").Count(&fileCount)
	if fileCount != 1 {
		t.Errorf("expected 1 file record, got %d", fileCount)
	}
}

// TestCompleteMultipartUploadDBOps_NoOrphanedRecordsOnDeleteFailure verifies
// that when the upload deletion fails, the file creation is also rolled back.
func TestCompleteMultipartUploadDBOps_NoOrphanedRecordsOnDeleteFailure(t *testing.T) {
	uc, db := newTxTestUsecase(t)
	ctx := context.Background()

	// Arrange: create upload record
	upload := &model.MultipartUpload{
		UserID:   "user-test-4",
		Bucket:   "test-bucket",
		Key:      "user-test-4/file.txt",
		UploadID: "test-upload-id-4",
		Status:   "uploading",
	}
	if err := db.Create(upload).Error; err != nil {
		t.Fatalf("create upload: %v", err)
	}

	// Use a failing uploadRepo for the delete operations
	uc.uploadRepo = &failingUploadRepo{inner: uc.uploadRepo, failOnDelete: true}

	// Act
	err := uc.completeMultipartUploadDBOps(ctx, "user-test-4", "test-bucket", "user-test-4/file.txt", "test-upload-id-4", "test-etag", 1024)

	// Assert: should have failed
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Assert: file record should NOT exist (rolled back)
	var fileCount int64
	db.Model(&model.File{}).Where("key = ?", "user-test-4/file.txt").Count(&fileCount)
	if fileCount != 0 {
		t.Errorf("expected 0 file records (rollback), got %d", fileCount)
	}

	// Assert: upload record should still exist (rolled back)
	var uploadCount int64
	db.Model(&model.MultipartUpload{}).Where("upload_id = ?", "test-upload-id-4").Count(&uploadCount)
	if uploadCount != 1 {
		t.Errorf("expected 1 upload record (rollback), got %d", uploadCount)
	}
}

// TestCompleteMultipartUploadDBOps_MultiplePartsCleanup verifies that all parts
// associated with an upload are deleted together with the upload record.
func TestCompleteMultipartUploadDBOps_MultiplePartsCleanup(t *testing.T) {
	uc, db := newTxTestUsecase(t)
	ctx := context.Background()

	// Arrange: create upload with multiple parts
	upload := &model.MultipartUpload{
		UserID:   "user-test-5",
		Bucket:   "test-bucket",
		Key:      "user-test-5/large-file.bin",
		UploadID: "test-upload-id-5",
		Status:   "uploading",
	}
	if err := db.Create(upload).Error; err != nil {
		t.Fatalf("create upload: %v", err)
	}
	for i := 1; i <= 5; i++ {
		part := &model.MultipartUploadPart{
			UploadID:   upload.ID,
			PartNumber: i,
			Size:       int64(i * 1024),
			ETag:       fmt.Sprintf("etag-part-%d", i),
		}
		if err := db.Create(part).Error; err != nil {
			t.Fatalf("create part %d: %v", i, err)
		}
	}

	// Act
	err := uc.completeMultipartUploadDBOps(ctx, "user-test-5", "test-bucket", "user-test-5/large-file.bin", "test-upload-id-5", "combined-etag", 15360)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert: file created with correct size
	var file model.File
	db.Where("key = ?", "user-test-5/large-file.bin").First(&file)
	if file.Size != 15360 {
		t.Errorf("file size: got %d, want 15360", file.Size)
	}

	// Assert: all 5 parts deleted
	var partCount int64
	db.Model(&model.MultipartUploadPart{}).Where("upload_id = ?", upload.ID).Count(&partCount)
	if partCount != 0 {
		t.Errorf("expected 0 parts, got %d", partCount)
	}

	// Assert: upload deleted
	var uploadCount int64
	db.Model(&model.MultipartUpload{}).Where("upload_id = ?", "test-upload-id-5").Count(&uploadCount)
	if uploadCount != 0 {
		t.Errorf("expected 0 uploads, got %d", uploadCount)
	}
}

// ============================================================
// Test helpers: mock repos that can be configured to fail
// ============================================================

// failingFileRepo wraps a FileRepo and makes Create always fail.
type failingFileRepo struct {
	inner repo.FileRepo
}

func (r *failingFileRepo) Create(ctx context.Context, file *model.File) error {
	return fmt.Errorf("simulated file creation failure")
}
func (r *failingFileRepo) GetByUserAndKey(ctx context.Context, userID, key string) (*model.File, error) {
	return r.inner.GetByUserAndKey(ctx, userID, key)
}
func (r *failingFileRepo) Update(ctx context.Context, file *model.File) error {
	return r.inner.Update(ctx, file)
}
func (r *failingFileRepo) Delete(ctx context.Context, id uuid.UUID) error {
	return r.inner.Delete(ctx, id)
}
func (r *failingFileRepo) DeleteByUserAndKey(ctx context.Context, userID, key string) error {
	return r.inner.DeleteByUserAndKey(ctx, userID, key)
}
func (r *failingFileRepo) SumSizeByUser(ctx context.Context, userID string) (int64, error) {
	return r.inner.SumSizeByUser(ctx, userID)
}
func (r *failingFileRepo) SumSizeByUserGrouped(ctx context.Context) (map[string]int64, error) {
	return r.inner.SumSizeByUserGrouped(ctx)
}
func (r *failingFileRepo) SumSizeByUserGroupedPaginated(ctx context.Context, cursor string, limit int) (map[string]int64, string, error) {
	return r.inner.SumSizeByUserGroupedPaginated(ctx, cursor, limit)
}
func (r *failingFileRepo) KeysExist(ctx context.Context, keys []string) (map[string]bool, error) {
	return r.inner.KeysExist(ctx, keys)
}

// failingUploadRepo wraps a MultipartUploadRepo and makes Delete fail.
type failingUploadRepo struct {
	inner        repo.MultipartUploadRepo
	failOnDelete bool
}

func (r *failingUploadRepo) Create(ctx context.Context, upload *model.MultipartUpload) error {
	return r.inner.Create(ctx, upload)
}
func (r *failingUploadRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.MultipartUpload, error) {
	return r.inner.GetByID(ctx, id)
}
func (r *failingUploadRepo) GetByUploadID(ctx context.Context, uploadID string) (*model.MultipartUpload, error) {
	return r.inner.GetByUploadID(ctx, uploadID)
}
func (r *failingUploadRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status string, uploadedParts int) error {
	return r.inner.UpdateStatus(ctx, id, status, uploadedParts)
}
func (r *failingUploadRepo) FindExpired(ctx context.Context, before time.Time) ([]*model.MultipartUpload, error) {
	return r.inner.FindExpired(ctx, before)
}
func (r *failingUploadRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if r.failOnDelete {
		return fmt.Errorf("simulated upload delete failure")
	}
	return r.inner.Delete(ctx, id)
}
func (r *failingUploadRepo) CountActiveByUser(ctx context.Context, userID string) (int64, error) {
	return r.inner.CountActiveByUser(ctx, userID)
}
func (r *failingUploadRepo) CreatePart(ctx context.Context, part *model.MultipartUploadPart) error {
	return r.inner.CreatePart(ctx, part)
}
func (r *failingUploadRepo) ListParts(ctx context.Context, uploadID uuid.UUID) ([]*model.MultipartUploadPart, error) {
	return r.inner.ListParts(ctx, uploadID)
}
func (r *failingUploadRepo) DeleteParts(ctx context.Context, uploadID uuid.UUID) error {
	return r.inner.DeleteParts(ctx, uploadID)
}
