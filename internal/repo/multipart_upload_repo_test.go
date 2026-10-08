package repo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

func TestMultipartUploadRepo(t *testing.T) {
	t.Run("Create_and_GetByID", func(t *testing.T) {
		db := setupTestDBWithModels(t, "multipart_upload_repo_create_", &model.MultipartUpload{}, &model.MultipartUploadPart{})
		repo := NewMultipartUploadRepo(db)
		ctx := context.Background()

		upload := &model.MultipartUpload{
			UserID:   "user-1",
			Key:      "test/large-file.bin",
			UploadID: "upload-123",
			Status:   "uploading",
		}
		if err := repo.Create(ctx, upload); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if upload.ID == uuid.Nil {
			t.Error("Create: expected non-nil ID")
		}

		got, err := repo.GetByID(ctx, upload.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.UploadID != upload.UploadID {
			t.Errorf("GetByID: got upload_id %q, want %q", got.UploadID, upload.UploadID)
		}
	})

	t.Run("GetByUploadID", func(t *testing.T) {
		db := setupTestDBWithModels(t, "multipart_upload_repo_getbyuploadid_", &model.MultipartUpload{}, &model.MultipartUploadPart{})
		repo := NewMultipartUploadRepo(db)
		ctx := context.Background()

		upload := &model.MultipartUpload{
			UserID:   "user-2",
			Key:      "test/file.bin",
			UploadID: "upload-456",
			Status:   "uploading",
		}
		if err := repo.Create(ctx, upload); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := repo.GetByUploadID(ctx, "upload-456")
		if err != nil {
			t.Fatalf("GetByUploadID: %v", err)
		}
		if got.Key != upload.Key {
			t.Errorf("GetByUploadID: got key %q, want %q", got.Key, upload.Key)
		}
	})

	t.Run("UpdateStatus", func(t *testing.T) {
		db := setupTestDBWithModels(t, "multipart_upload_repo_updatestatus_", &model.MultipartUpload{}, &model.MultipartUploadPart{})
		repo := NewMultipartUploadRepo(db)
		ctx := context.Background()

		upload := &model.MultipartUpload{
			UserID:   "user-3",
			Key:      "test/status.bin",
			UploadID: "upload-789",
			Status:   "uploading",
		}
		if err := repo.Create(ctx, upload); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := repo.UpdateStatus(ctx, upload.ID, "completed", 5); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}

		got, err := repo.GetByID(ctx, upload.ID)
		if err != nil {
			t.Fatalf("GetByID after update: %v", err)
		}
		if got.Status != "completed" {
			t.Errorf("UpdateStatus: got status %q, want 'completed'", got.Status)
		}
		if got.UploadedParts != 5 {
			t.Errorf("UpdateStatus: got uploaded_parts %d, want 5", got.UploadedParts)
		}
	})

	t.Run("FindExpired", func(t *testing.T) {
		db := setupTestDBWithModels(t, "multipart_upload_repo_findexpired_", &model.MultipartUpload{}, &model.MultipartUploadPart{})
		repo := NewMultipartUploadRepo(db)
		ctx := context.Background()

		// Create expired upload
		expired := &model.MultipartUpload{
			UserID:    "user-4",
			Key:       "test/expired.bin",
			UploadID:  "upload-expired",
			Status:    "uploading",
			ExpiresAt: time.Now().Add(-1 * time.Hour),
		}
		if err := repo.Create(ctx, expired); err != nil {
			t.Fatalf("Create expired: %v", err)
		}

		// Create active upload
		active := &model.MultipartUpload{
			UserID:    "user-4",
			Key:       "test/active.bin",
			UploadID:  "upload-active",
			Status:    "uploading",
			ExpiresAt: time.Now().Add(1 * time.Hour),
		}
		if err := repo.Create(ctx, active); err != nil {
			t.Fatalf("Create active: %v", err)
		}

		expiredUploads, err := repo.FindExpired(ctx, time.Now())
		if err != nil {
			t.Fatalf("FindExpired: %v", err)
		}
		if len(expiredUploads) != 1 {
			t.Errorf("FindExpired: got %d uploads, want 1", len(expiredUploads))
		}
	})

	t.Run("CountActiveByUser", func(t *testing.T) {
		db := setupTestDBWithModels(t, "multipart_upload_repo_countactive_", &model.MultipartUpload{}, &model.MultipartUploadPart{})
		repo := NewMultipartUploadRepo(db)
		ctx := context.Background()

		// Create multiple active uploads for user-5
		for i := 0; i < 3; i++ {
			upload := &model.MultipartUpload{
				UserID:   "user-5",
				Key:      "test/active-" + string(rune('0'+i)) + ".bin",
				UploadID: "upload-active-" + string(rune('0'+i)),
				Status:   "uploading",
			}
			if err := repo.Create(ctx, upload); err != nil {
				t.Fatalf("Create upload %d: %v", i, err)
			}
		}

		count, err := repo.CountActiveByUser(ctx, "user-5")
		if err != nil {
			t.Fatalf("CountActiveByUser: %v", err)
		}
		if count != 3 {
			t.Errorf("CountActiveByUser: got %d, want 3", count)
		}
	})

	t.Run("Parts_operations", func(t *testing.T) {
		db := setupTestDBWithModels(t, "multipart_upload_repo_parts_", &model.MultipartUpload{}, &model.MultipartUploadPart{})
		repo := NewMultipartUploadRepo(db)
		ctx := context.Background()

		upload := &model.MultipartUpload{
			UserID:   "user-6",
			Key:      "test/parts.bin",
			UploadID: "upload-parts",
			Status:   "uploading",
		}
		if err := repo.Create(ctx, upload); err != nil {
			t.Fatalf("Create upload: %v", err)
		}

		// Create parts
		for i := 1; i <= 3; i++ {
			part := &model.MultipartUploadPart{
				UploadID:   upload.ID,
				PartNumber: i,
				Size:       int64(5 * 1024 * 1024),
				ETag:       "etag-" + string(rune('0'+i)),
			}
			if err := repo.CreatePart(ctx, part); err != nil {
				t.Fatalf("CreatePart %d: %v", i, err)
			}
		}

		// List parts
		parts, err := repo.ListParts(ctx, upload.ID)
		if err != nil {
			t.Fatalf("ListParts: %v", err)
		}
		if len(parts) != 3 {
			t.Errorf("ListParts: got %d parts, want 3", len(parts))
		}

		// Delete parts
		if err := repo.DeleteParts(ctx, upload.ID); err != nil {
			t.Fatalf("DeleteParts: %v", err)
		}

		parts, err = repo.ListParts(ctx, upload.ID)
		if err != nil {
			t.Fatalf("ListParts after delete: %v", err)
		}
		if len(parts) != 0 {
			t.Errorf("ListParts after delete: got %d parts, want 0", len(parts))
		}
	})
}
