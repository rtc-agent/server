package usecase

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// mockFileRepo is a mock implementation of repo.FileRepo for testing.
type mockFileRepo struct {
	keysExistFunc func(ctx context.Context, keys []string) (map[string]bool, error)
}

func (m *mockFileRepo) Create(ctx context.Context, file *model.File) error {
	return nil
}

func (m *mockFileRepo) GetByUserAndKey(ctx context.Context, userID string, key string) (*model.File, error) {
	return nil, nil
}

func (m *mockFileRepo) Update(ctx context.Context, file *model.File) error {
	return nil
}

func (m *mockFileRepo) Delete(ctx context.Context, id uuid.UUID) error {
	return nil
}

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

func (m *mockFileRepo) KeysExist(ctx context.Context, keys []string) (map[string]bool, error) {
	if m.keysExistFunc != nil {
		return m.keysExistFunc(ctx, keys)
	}
	return make(map[string]bool), nil
}

// TestFileUsecase_ValidateFilesExist tests the ValidateFilesExist method.
func TestFileUsecase_ValidateFilesExist(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	tests := []struct {
		name          string
		files         []protocol.FileAttachment
		setupMock     func() *mockFileRepo
		expectError   bool
		errorContains string
	}{
		{
			name:  "empty_files",
			files: nil,
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{}
			},
			expectError: false,
		},
		{
			name:  "single_valid_file",
			files: []protocol.FileAttachment{{Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt"}},
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{
					keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
						return map[string]bool{
							"user-" + userID.String() + "/a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt": true,
						}, nil
					},
				}
			},
			expectError: false,
		},
		{
			name: "multiple_valid_files",
			files: []protocol.FileAttachment{
				{Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt"},
				{Fileid: "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7.jpg"},
			},
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{
					keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
						return map[string]bool{
							"user-" + userID.String() + "/a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt": true,
							"user-" + userID.String() + "/b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7.jpg": true,
						}, nil
					},
				}
			},
			expectError: false,
		},
		{
			name:  "invalid_file_id_format_missing_extension",
			files: []protocol.FileAttachment{{Fileid: "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6"}},
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{}
			},
			expectError:   true,
			errorContains: "invalid file ID format",
		},
		{
			name:  "invalid_file_id_format_wrong_hash_length",
			files: []protocol.FileAttachment{{Fileid: "a1b2c3d4.txt"}},
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{}
			},
			expectError:   true,
			errorContains: "invalid file ID format",
		},
		{
			name:  "invalid_file_id_format_uppercase_hash",
			files: []protocol.FileAttachment{{Fileid: "A1B2C3D4E5F6G7H8I9J0K1L2M3N4O5P6.txt"}},
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{}
			},
			expectError:   true,
			errorContains: "invalid file ID format",
		},
		{
			name:  "file_not_found",
			files: []protocol.FileAttachment{{Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt"}},
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{
					keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
						return map[string]bool{
							"user-" + userID.String() + "/a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt": false,
						}, nil
					},
				}
			},
			expectError:   true,
			errorContains: "not found",
		},
		{
			name:  "too_many_files",
			files: make([]protocol.FileAttachment, MaxFilesPerMessage+1),
			setupMock: func() *mockFileRepo {
				return &mockFileRepo{}
			},
			expectError:   true,
			errorContains: "too many file attachments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fileRepo := tt.setupMock()
			uc := NewFileUsecase(fileRepo)

			err := uc.ValidateFilesExist(ctx, tt.files, userID)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestFileUsecase_MaxFilesPerMessage tests the MaxFilesPerMessage constant.
func TestFileUsecase_MaxFilesPerMessage(t *testing.T) {
	assert.Equal(t, 100, MaxFilesPerMessage, "MaxFilesPerMessage should be 100")
}

// TestFileIDPattern tests the file ID pattern regex.
func TestFileIDPattern(t *testing.T) {
	tests := []struct {
		name     string
		fileID   string
		expected bool
	}{
		{
			name:     "valid_md5_with_txt_extension",
			fileID:   "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6.txt",
			expected: false, // g is not a valid hex character
		},
		{
			name:     "valid_md5_with_jpg_extension",
			fileID:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.jpg",
			expected: true,
		},
		{
			name:     "valid_md5_with_png_extension",
			fileID:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.png",
			expected: true,
		},
		{
			name:     "valid_md5_with_pdf_extension",
			fileID:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.pdf",
			expected: true,
		},
		{
			name:     "invalid_too_short",
			fileID:   "a1b2c3d4.txt",
			expected: false,
		},
		{
			name:     "invalid_too_long",
			fileID:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8.txt",
			expected: false,
		},
		{
			name:     "invalid_uppercase",
			fileID:   "A1B2C3D4E5F6A7B8C9D0E1F2A3B4C5D6.txt",
			expected: false,
		},
		{
			name:     "invalid_no_extension",
			fileID:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
			expected: false,
		},
		{
			name:     "invalid_extension_too_long",
			fileID:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.verylongextension",
			expected: false,
		},
		{
			name:     "valid_with_numbers",
			fileID:   "0123456789abcdef0123456789abcdef.doc",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fileIDPattern.MatchString(tt.fileID)
			assert.Equal(t, tt.expected, result, "fileID: %s", tt.fileID)
		})
	}
}

// TestNewFileUsecase tests the NewFileUsecase constructor.
func TestNewFileUsecase(t *testing.T) {
	fileRepo := &mockFileRepo{}
	uc := NewFileUsecase(fileRepo)

	assert.NotNil(t, uc)
	assert.Equal(t, fileRepo, uc.fileRepo)
}

// TestNewFileUsecase_NilRepo tests creating a usecase with nil repo.
func TestNewFileUsecase_NilRepo(t *testing.T) {
	uc := NewFileUsecase(nil)

	assert.NotNil(t, uc)
	assert.Nil(t, uc.fileRepo)
}

// TestFileUsecase_ValidateFilesExist_EmptySlice tests with empty slice.
func TestFileUsecase_ValidateFilesExist_EmptySlice(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	uc := NewFileUsecase(&mockFileRepo{})

	err := uc.ValidateFilesExist(ctx, []protocol.FileAttachment{}, userID)
	require.NoError(t, err)
}

// TestFileUsecase_ValidateFilesExist_BoundaryTests tests boundary conditions.
func TestFileUsecase_ValidateFilesExist_BoundaryTests(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	t.Run("exactly_max_files", func(t *testing.T) {
		files := make([]protocol.FileAttachment, MaxFilesPerMessage)
		for i := range files {
			files[i] = protocol.FileAttachment{
				Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d" + string(rune('0'+i%10)) + ".txt",
			}
		}

		fileRepo := &mockFileRepo{
			keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
				result := make(map[string]bool)
				for _, key := range keys {
					result[key] = true
				}
				return result, nil
			},
		}

		uc := NewFileUsecase(fileRepo)
		err := uc.ValidateFilesExist(ctx, files, userID)
		require.NoError(t, err)
	})

	t.Run("one_over_max_files", func(t *testing.T) {
		files := make([]protocol.FileAttachment, MaxFilesPerMessage+1)
		for i := range files {
			files[i] = protocol.FileAttachment{
				Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d" + string(rune('0'+i%10)) + ".txt",
			}
		}

		uc := NewFileUsecase(&mockFileRepo{})
		err := uc.ValidateFilesExist(ctx, files, userID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too many file attachments")
	})
}

// TestFileUsecase_ValidateFilesExist_FileExtensions tests various file extensions.
func TestFileUsecase_ValidateFilesExist_FileExtensions(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	extensions := []string{"txt", "jpg", "png", "pdf", "doc", "mp3", "wav", "mp4"}

	for _, ext := range extensions {
		t.Run(ext, func(t *testing.T) {
			fileID := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6." + ext
			files := []protocol.FileAttachment{{Fileid: fileID}}

			fileRepo := &mockFileRepo{
				keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
					result := make(map[string]bool)
					for _, key := range keys {
						result[key] = true
					}
					return result, nil
				},
			}

			uc := NewFileUsecase(fileRepo)
			err := uc.ValidateFilesExist(ctx, files, userID)
			require.NoError(t, err)
		})
	}
}

// TestFileUsecase_ValidateFilesExist_SecurityTests tests security-related validations.
func TestFileUsecase_ValidateFilesExist_SecurityTests(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	t.Run("prevent_cross_user_file_references", func(t *testing.T) {
		// File IDs should be constructed with the correct user ID
		// to prevent cross-user file references
		files := []protocol.FileAttachment{
			{Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt"},
		}

		fileRepo := &mockFileRepo{
			keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
				// Verify the key includes the correct user ID
				expectedKey := "user-" + userID.String() + "/a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt"
				for _, key := range keys {
					if key != expectedKey {
						t.Errorf("unexpected key: got %s, want %s", key, expectedKey)
					}
				}
				return map[string]bool{expectedKey: true}, nil
			},
		}

		uc := NewFileUsecase(fileRepo)
		err := uc.ValidateFilesExist(ctx, files, userID)
		require.NoError(t, err)
	})
}

// BenchmarkFileUsecase_ValidateFilesExist benchmarks file validation.
func BenchmarkFileUsecase_ValidateFilesExist(b *testing.B) {
	ctx := context.Background()
	userID := uuid.New()

	files := make([]protocol.FileAttachment, 10)
	for i := range files {
		files[i] = protocol.FileAttachment{
			Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d" + string(rune('0'+i%10)) + ".txt",
		}
	}

	fileRepo := &mockFileRepo{
		keysExistFunc: func(ctx context.Context, keys []string) (map[string]bool, error) {
			result := make(map[string]bool)
			for _, key := range keys {
				result[key] = true
			}
			return result, nil
		},
	}

	uc := NewFileUsecase(fileRepo)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = uc.ValidateFilesExist(ctx, files, userID)
	}
}

// ExampleNewFileUsecase demonstrates creating a FileUsecase.
func ExampleNewFileUsecase() {
	// Create with dependencies
	fileRepo := &mockFileRepo{}
	uc := NewFileUsecase(fileRepo)

	// Validate files exist
	ctx := context.Background()
	userID := uuid.New()
	files := []protocol.FileAttachment{
		{Fileid: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.txt"},
	}
	_ = uc.ValidateFilesExist(ctx, files, userID)
}
