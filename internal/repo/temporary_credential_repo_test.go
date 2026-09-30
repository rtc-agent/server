package repo

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

func TestTemporaryCredentialRepo(t *testing.T) {
	t.Run("Create_and_GetByAccessKeyID", func(t *testing.T) {
		db := setupTestDBWithModels(t, "temporary_credential_repo_create_", &model.TemporaryCredential{})
		repo := NewTemporaryCredentialRepo(db)
		ctx := context.Background()

		cred := &model.TemporaryCredential{
			UserID:          "user-1",
			AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			SessionToken:    "encrypted-session-token",
			ExpiresAt:       time.Now().Add(1 * time.Hour),
		}
		if err := repo.Create(ctx, cred); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if cred.ID == uuid.Nil {
			t.Error("Create: expected non-nil ID")
		}

		got, err := repo.GetByAccessKeyID(ctx, "AKIAIOSFODNN7EXAMPLE")
		if err != nil {
			t.Fatalf("GetByAccessKeyID: %v", err)
		}
		if got.AccessKeyID != cred.AccessKeyID {
			t.Errorf("GetByAccessKeyID: got access_key_id %q, want %q", got.AccessKeyID, cred.AccessKeyID)
		}
		if got.SessionToken != cred.SessionToken {
			t.Errorf("GetByAccessKeyID: got session_token %q, want %q", got.SessionToken, cred.SessionToken)
		}
	})

	t.Run("FindExpired", func(t *testing.T) {
		db := setupTestDBWithModels(t, "temporary_credential_repo_findexpired_", &model.TemporaryCredential{})
		repo := NewTemporaryCredentialRepo(db)
		ctx := context.Background()

		// Create expired credential
		expired := &model.TemporaryCredential{
			UserID:          "user-2",
			AccessKeyID:     "AKIAIOSFODNN7EXPIRED",
			SecretAccessKey: "secret-expired",
			SessionToken:    "token-expired",
			ExpiresAt:       time.Now().Add(-1 * time.Hour),
		}
		if err := repo.Create(ctx, expired); err != nil {
			t.Fatalf("Create expired: %v", err)
		}

		// Create active credential
		active := &model.TemporaryCredential{
			UserID:          "user-2",
			AccessKeyID:     "AKIAIOSFODNN7ACTIVE",
			SecretAccessKey: "secret-active",
			SessionToken:    "token-active",
			ExpiresAt:       time.Now().Add(1 * time.Hour),
		}
		if err := repo.Create(ctx, active); err != nil {
			t.Fatalf("Create active: %v", err)
		}

		expiredCreds, err := repo.FindExpired(ctx, time.Now())
		if err != nil {
			t.Fatalf("FindExpired: %v", err)
		}
		if len(expiredCreds) != 1 {
			t.Errorf("FindExpired: got %d credentials, want 1", len(expiredCreds))
		}
	})

	t.Run("Delete", func(t *testing.T) {
		db := setupTestDBWithModels(t, "temporary_credential_repo_delete_", &model.TemporaryCredential{})
		repo := NewTemporaryCredentialRepo(db)
		ctx := context.Background()

		cred := &model.TemporaryCredential{
			UserID:          "user-3",
			AccessKeyID:     "AKIAIOSFODNN7DELETE",
			SecretAccessKey: "secret-delete",
			SessionToken:    "token-delete",
			ExpiresAt:       time.Now().Add(1 * time.Hour),
		}
		if err := repo.Create(ctx, cred); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := repo.Delete(ctx, cred.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err := repo.GetByAccessKeyID(ctx, "AKIAIOSFODNN7DELETE")
		if err == nil {
			t.Error("GetByAccessKeyID after delete: expected error, got nil")
		}
	})

	t.Run("DeleteExpired", func(t *testing.T) {
		db := setupTestDBWithModels(t, "temporary_credential_repo_deleteexpired_", &model.TemporaryCredential{})
		repo := NewTemporaryCredentialRepo(db)
		ctx := context.Background()

		// Create multiple expired credentials
		for i := 0; i < 3; i++ {
			cred := &model.TemporaryCredential{
				UserID:          "user-4",
				AccessKeyID:     "AKIAIOSFODNN7BATCH" + string(rune('0'+i)),
				SecretAccessKey: "secret-batch-" + string(rune('0'+i)),
				SessionToken:    "token-batch-" + string(rune('0'+i)),
				ExpiresAt:       time.Now().Add(-1 * time.Hour),
			}
			if err := repo.Create(ctx, cred); err != nil {
				t.Fatalf("Create credential %d: %v", i, err)
			}
		}

		// Create active credential
		active := &model.TemporaryCredential{
			UserID:          "user-4",
			AccessKeyID:     "AKIAIOSFODNN7BATCHACTIVE",
			SecretAccessKey: "secret-batch-active",
			SessionToken:    "token-batch-active",
			ExpiresAt:       time.Now().Add(1 * time.Hour),
		}
		if err := repo.Create(ctx, active); err != nil {
			t.Fatalf("Create active: %v", err)
		}

		deleted, err := repo.DeleteExpired(ctx, time.Now())
		if err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		if deleted != 3 {
			t.Errorf("DeleteExpired: deleted %d credentials, want 3", deleted)
		}

		// Verify active credential still exists
		_, err = repo.GetByAccessKeyID(ctx, "AKIAIOSFODNN7BATCHACTIVE")
		if err != nil {
			t.Errorf("GetByAccessKeyID for active credential: %v", err)
		}
	})
}
