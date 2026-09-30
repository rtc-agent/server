package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

func TestFileRepo(t *testing.T) {
	db := setupTestDBWithModels(t, "file_repo_", &model.File{})
	repo := NewFileRepo(db)
	ctx := context.Background()

	t.Run("Create and GetByUserAndKey", func(t *testing.T) {
		file := &model.File{
			UserID:      "user-1",
			Key:         "test/file.txt",
			Size:        1024,
			ContentType: "text/plain",
			ETag:        "abc123",
		}
		if err := repo.Create(ctx, file); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if file.ID == uuid.Nil {
			t.Error("Create: expected non-nil ID")
		}

		got, err := repo.GetByUserAndKey(ctx, "user-1", "test/file.txt")
		if err != nil {
			t.Fatalf("GetByUserAndKey: %v", err)
		}
		if got.Key != file.Key {
			t.Errorf("GetByUserAndKey: got key %q, want %q", got.Key, file.Key)
		}
		if got.Size != file.Size {
			t.Errorf("GetByUserAndKey: got size %d, want %d", got.Size, file.Size)
		}
	})

	t.Run("Update", func(t *testing.T) {
		file := &model.File{
			UserID:      "user-2",
			Key:         "test/update.txt",
			Size:        100,
			ContentType: "text/plain",
		}
		if err := repo.Create(ctx, file); err != nil {
			t.Fatalf("Create: %v", err)
		}

		file.Size = 200
		if err := repo.Update(ctx, file); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := repo.GetByUserAndKey(ctx, "user-2", "test/update.txt")
		if err != nil {
			t.Fatalf("GetByUserAndKey after update: %v", err)
		}
		if got.Size != 200 {
			t.Errorf("Update: got size %d, want 200", got.Size)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		file := &model.File{
			UserID:      "user-3",
			Key:         "test/delete.txt",
			Size:        100,
			ContentType: "text/plain",
		}
		if err := repo.Create(ctx, file); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := repo.Delete(ctx, file.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err := repo.GetByUserAndKey(ctx, "user-3", "test/delete.txt")
		if err == nil {
			t.Error("GetByUserAndKey after delete: expected error, got nil")
		}
	})

	t.Run("DeleteByUserAndKey", func(t *testing.T) {
		file := &model.File{
			UserID:      "user-4",
			Key:         "test/delete-by-key.txt",
			Size:        100,
			ContentType: "text/plain",
		}
		if err := repo.Create(ctx, file); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := repo.DeleteByUserAndKey(ctx, "user-4", "test/delete-by-key.txt"); err != nil {
			t.Fatalf("DeleteByUserAndKey: %v", err)
		}

		_, err := repo.GetByUserAndKey(ctx, "user-4", "test/delete-by-key.txt")
		if err == nil {
			t.Error("GetByUserAndKey after DeleteByUserAndKey: expected error, got nil")
		}
	})

	t.Run("SumSizeByUser", func(t *testing.T) {
		// Create multiple files for user-5
		for i := 0; i < 3; i++ {
			file := &model.File{
				UserID:      "user-5",
				Key:         "test/sum-" + string(rune('0'+i)) + ".txt",
				Size:        int64(100 * (i + 1)),
				ContentType: "text/plain",
			}
			if err := repo.Create(ctx, file); err != nil {
				t.Fatalf("Create file %d: %v", i, err)
			}
		}

		total, err := repo.SumSizeByUser(ctx, "user-5")
		if err != nil {
			t.Fatalf("SumSizeByUser: %v", err)
		}
		// 100 + 200 + 300 = 600
		if total != 600 {
			t.Errorf("SumSizeByUser: got %d, want 600", total)
		}
	})

	t.Run("SumSizeByUserGrouped", func(t *testing.T) {
		// Create files for user-6 and user-7
		file1 := &model.File{
			UserID:      "user-6",
			Key:         "test/grouped-1.txt",
			Size:        100,
			ContentType: "text/plain",
		}
		file2 := &model.File{
			UserID:      "user-7",
			Key:         "test/grouped-2.txt",
			Size:        200,
			ContentType: "text/plain",
		}
		if err := repo.Create(ctx, file1); err != nil {
			t.Fatalf("Create file1: %v", err)
		}
		if err := repo.Create(ctx, file2); err != nil {
			t.Fatalf("Create file2: %v", err)
		}

		grouped, err := repo.SumSizeByUserGrouped(ctx)
		if err != nil {
			t.Fatalf("SumSizeByUserGrouped: %v", err)
		}

		if grouped["user-6"] != 100 {
			t.Errorf("SumSizeByUserGrouped user-6: got %d, want 100", grouped["user-6"])
		}
		if grouped["user-7"] != 200 {
			t.Errorf("SumSizeByUserGrouped user-7: got %d, want 200", grouped["user-7"])
		}
	})
}
