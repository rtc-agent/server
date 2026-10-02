package primitives

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// mockFileRepo implements repo.FileRepo for testing.
type mockFileRepo struct {
	existingKeys map[string]bool
}

func (m *mockFileRepo) KeysExist(ctx context.Context, keys []string) (map[string]bool, error) {
	result := make(map[string]bool)
	for _, key := range keys {
		result[key] = m.existingKeys[key]
	}
	return result, nil
}

// Stub implementations for unused FileRepo methods.
func (m *mockFileRepo) Create(ctx context.Context, file *model.File) error { return nil }
func (m *mockFileRepo) GetByUserAndKey(ctx context.Context, userID string, key string) (*model.File, error) {
	return nil, nil
}
func (m *mockFileRepo) Update(ctx context.Context, file *model.File) error { return nil }
func (m *mockFileRepo) Delete(ctx context.Context, id uuid.UUID) error     { return nil }
func (m *mockFileRepo) DeleteByUserAndKey(ctx context.Context, userID string, key string) error {
	return nil
}
func (m *mockFileRepo) SumSizeByUser(ctx context.Context, userID string) (int64, error) {
	return 0, nil
}
func (m *mockFileRepo) SumSizeByUserGrouped(ctx context.Context) (map[string]int64, error) {
	return nil, nil
}
func (m *mockFileRepo) SumSizeByUserGroupedPaginated(ctx context.Context, cursor string, limit int) (map[string]int64, string, error) {
	return nil, "", nil
}

func TestValidateFilesExist(t *testing.T) {
	userID := uuid.New()
	otherUserID := uuid.New()

	tests := []struct {
		name        string
		files       []protocol.FileAttachment
		existing    map[string]bool
		expectError bool
		errorMsg    string
	}{
		{
			name:        "nil files",
			files:       nil,
			existing:    map[string]bool{},
			expectError: false,
		},
		{
			name:        "empty files",
			files:       []protocol.FileAttachment{},
			existing:    map[string]bool{},
			expectError: false,
		},
		{
			name: "all files exist",
			files: []protocol.FileAttachment{
				{Fileid: "user-" + userID.String() + "/file1.jpg"},
				{Fileid: "user-" + userID.String() + "/file2.png"},
			},
			existing: map[string]bool{
				"user-" + userID.String() + "/file1.jpg": true,
				"user-" + userID.String() + "/file2.png": true,
			},
			expectError: false,
		},
		{
			name: "file not found",
			files: []protocol.FileAttachment{
				{Fileid: "user-" + userID.String() + "/file1.jpg"},
				{Fileid: "user-" + userID.String() + "/missing.png"},
			},
			existing: map[string]bool{
				"user-" + userID.String() + "/file1.jpg": true,
			},
			expectError: true,
			errorMsg:    "files not found",
		},
		{
			name: "file belongs to other user",
			files: []protocol.FileAttachment{
				{Fileid: "user-" + otherUserID.String() + "/file1.jpg"},
			},
			existing: map[string]bool{
				"user-" + otherUserID.String() + "/file1.jpg": true,
			},
			expectError: true,
			errorMsg:    "does not belong to user",
		},
		{
			name: "mixed: some belong to other user",
			files: []protocol.FileAttachment{
				{Fileid: "user-" + userID.String() + "/file1.jpg"},
				{Fileid: "user-" + otherUserID.String() + "/file2.png"},
			},
			existing: map[string]bool{
				"user-" + userID.String() + "/file1.jpg":      true,
				"user-" + otherUserID.String() + "/file2.png": true,
			},
			expectError: true,
			errorMsg:    "does not belong to user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &mockFileRepo{existingKeys: tt.existing}
			err := ValidateFilesExist(context.Background(), repo, tt.files, userID)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errorMsg)
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}
