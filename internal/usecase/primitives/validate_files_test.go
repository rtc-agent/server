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
	userIDStr := userID.String()

	// Valid file IDs (format: {md5}.{ext})
	// MD5 must be 32 hex characters (a-f, 0-9)
	validFileID1 := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.jpg"
	validFileID2 := "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7.png"
	validFileID3 := "c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8.pdf"

	// Full keys as stored in database
	fullKey1 := "user-" + userIDStr + "/" + validFileID1
	fullKey2 := "user-" + userIDStr + "/" + validFileID2

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
				{Fileid: validFileID1},
				{Fileid: validFileID2},
			},
			existing: map[string]bool{
				fullKey1: true,
				fullKey2: true,
			},
			expectError: false,
		},
		{
			name: "file not found",
			files: []protocol.FileAttachment{
				{Fileid: validFileID1},
				{Fileid: validFileID2},
			},
			existing: map[string]bool{
				fullKey1: true,
				// fullKey2 is missing
			},
			expectError: true,
			errorMsg:    "files not found",
		},
		{
			name: "invalid file ID format - no extension",
			files: []protocol.FileAttachment{
				{Fileid: "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6"},
			},
			existing:    map[string]bool{},
			expectError: true,
			errorMsg:    "invalid file ID format",
		},
		{
			name: "invalid file ID format - wrong md5 length",
			files: []protocol.FileAttachment{
				{Fileid: "abc.jpg"},
			},
			existing:    map[string]bool{},
			expectError: true,
			errorMsg:    "invalid file ID format",
		},
		{
			name: "invalid file ID format - contains user prefix",
			files: []protocol.FileAttachment{
				{Fileid: fullKey1}, // Should NOT include user prefix
			},
			existing:    map[string]bool{},
			expectError: true,
			errorMsg:    "invalid file ID format",
		},
		{
			name: "empty fileid - invalid format",
			files: []protocol.FileAttachment{
				{Fileid: ""},
			},
			existing:    map[string]bool{},
			expectError: true,
			errorMsg:    "invalid file ID format",
		},
		{
			name:        "too many files - exceeds limit",
			files:       make([]protocol.FileAttachment, MaxFilesPerMessage+1),
			existing:    map[string]bool{},
			expectError: true,
			errorMsg:    "too many file attachments",
		},
		{
			name: "valid file ID but not in database",
			files: []protocol.FileAttachment{
				{Fileid: validFileID3},
			},
			existing: map[string]bool{
				// fullKey3 is not in the map
			},
			expectError: true,
			errorMsg:    "files not found",
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
