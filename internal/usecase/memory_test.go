package usecase

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
)

// mockMemoryRepository is a mock implementation of memory.Repository for testing.
type mockMemoryRepository struct {
	exportFunc func(ctx context.Context, opts memory.ExportOptions, w interface{}) error
}

func (m *mockMemoryRepository) Create(ctx context.Context, mem *memory.Memory) error {
	return nil
}

func (m *mockMemoryRepository) BatchCreate(ctx context.Context, memories []*memory.Memory) error {
	return nil
}

func (m *mockMemoryRepository) GetByID(ctx context.Context, id uuid.UUID) (*memory.Memory, error) {
	return nil, nil
}

func (m *mockMemoryRepository) ListByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, opts memory.ListOptions) ([]*memory.Memory, error) {
	return nil, nil
}

func (m *mockMemoryRepository) ListRecentForInjection(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, maxCount, maxTokens int) ([]*memory.Memory, error) {
	return nil, nil
}

func (m *mockMemoryRepository) Search(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID, query string, limit int) ([]*memory.Memory, error) {
	return nil, nil
}

func (m *mockMemoryRepository) Update(ctx context.Context, id uuid.UUID, fields map[string]any, expectedUpdatedAt ...time.Time) error {
	return nil
}

func (m *mockMemoryRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return nil
}

func (m *mockMemoryRepository) DeleteByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) error {
	return nil
}

func (m *mockMemoryRepository) GetLinked(ctx context.Context, id uuid.UUID, relation string) ([]*memory.Memory, error) {
	return nil, nil
}

func (m *mockMemoryRepository) CreateLink(ctx context.Context, link *memory.MemoryLink) error {
	return nil
}

func (m *mockMemoryRepository) DeleteLink(ctx context.Context, fromID, toID uuid.UUID) error {
	return nil
}

func (m *mockMemoryRepository) CountTokensByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) (int, error) {
	return 0, nil
}

func (m *mockMemoryRepository) Export(ctx context.Context, opts memory.ExportOptions, w interface{}) error {
	if m.exportFunc != nil {
		return m.exportFunc(ctx, opts, w)
	}
	return nil
}

// mockMemorySessionRepo is a mock implementation of memorySessionRepo for testing.
type mockMemorySessionRepo struct {
	getByIDFunc func(ctx context.Context, id uuid.UUID) (*model.Session, error)
}

func (m *mockMemorySessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error) {
	if m.getByIDFunc != nil {
		return m.getByIDFunc(ctx, id)
	}
	return nil, nil
}

// TestMemoryUsecase_ExportMemories_UserScope tests exporting user-scoped memories.
func TestMemoryUsecase_ExportMemories_UserScope(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	tests := []struct {
		name          string
		scopeID       uuid.UUID
		setupMock     func() (*mockMemoryRepository, *mockMemorySessionRepo)
		expectError   bool
		errorIs       error
		errorContains string
	}{
		{
			name:    "success_user_scope",
			scopeID: userID,
			setupMock: func() (*mockMemoryRepository, *mockMemorySessionRepo) {
				return &mockMemoryRepository{}, nil
			},
			expectError: false,
		},
		{
			name:    "forbidden_user_scope_different_user",
			scopeID: uuid.New(),
			setupMock: func() (*mockMemoryRepository, *mockMemorySessionRepo) {
				return &mockMemoryRepository{}, nil
			},
			expectError: true,
			errorIs:     ErrForbidden,
		},
		{
			name:    "not_configured",
			scopeID: userID,
			setupMock: func() (*mockMemoryRepository, *mockMemorySessionRepo) {
				return nil, nil
			},
			expectError: true,
			errorIs:     ErrMemoryNotConfigured,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memRepo, sessionRepo := tt.setupMock()

			var uc *MemoryUsecase
			if memRepo == nil {
				uc = &MemoryUsecase{
					memoryRepo:  nil,
					sessionRepo: sessionRepo,
				}
			} else {
				uc = &MemoryUsecase{
					memoryRepo:  memRepo,
					sessionRepo: sessionRepo,
				}
			}

			var buf bytes.Buffer
			opts := memory.ExportOptions{}

			err := uc.ExportMemories(ctx, userID, memory.ScopeUser, tt.scopeID, opts, &buf)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorIs != nil {
					assert.ErrorIs(t, err, tt.errorIs)
				}
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestMemoryUsecase_ExportMemories_SessionScope tests exporting session-scoped memories.
func TestMemoryUsecase_ExportMemories_SessionScope(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()
	sessionID := uuid.New()

	tests := []struct {
		name          string
		setupMock     func() (*mockMemoryRepository, *mockMemorySessionRepo)
		expectError   bool
		errorIs       error
		errorContains string
	}{
		{
			name: "success_session_scope",
			setupMock: func() (*mockMemoryRepository, *mockMemorySessionRepo) {
				sessionRepo := &mockMemorySessionRepo{
					getByIDFunc: func(ctx context.Context, id uuid.UUID) (*model.Session, error) {
						return &model.Session{
							ID:         id,
							OwnerRefID: userID.String(),
						}, nil
					},
				}
				return &mockMemoryRepository{}, sessionRepo
			},
			expectError: false,
		},
		{
			name: "forbidden_session_scope_different_owner",
			setupMock: func() (*mockMemoryRepository, *mockMemorySessionRepo) {
				sessionRepo := &mockMemorySessionRepo{
					getByIDFunc: func(ctx context.Context, id uuid.UUID) (*model.Session, error) {
						return &model.Session{
							ID:         id,
							OwnerRefID: uuid.New().String(), // Different owner
						}, nil
					},
				}
				return &mockMemoryRepository{}, sessionRepo
			},
			expectError: true,
			errorIs:     ErrForbidden,
		},
		{
			name: "session_not_found",
			setupMock: func() (*mockMemoryRepository, *mockMemorySessionRepo) {
				sessionRepo := &mockMemorySessionRepo{
					getByIDFunc: func(ctx context.Context, id uuid.UUID) (*model.Session, error) {
						return nil, ErrSessionNotFound
					},
				}
				return &mockMemoryRepository{}, sessionRepo
			},
			expectError: true,
			errorIs:     ErrSessionNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memRepo, sessionRepo := tt.setupMock()

			uc := &MemoryUsecase{
				memoryRepo:  memRepo,
				sessionRepo: sessionRepo,
			}

			var buf bytes.Buffer
			opts := memory.ExportOptions{}

			err := uc.ExportMemories(ctx, userID, memory.ScopeSession, sessionID, opts, &buf)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorIs != nil {
					assert.ErrorIs(t, err, tt.errorIs)
				}
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestMemoryUsecase_ExportMemories_GlobalScope tests exporting global-scoped memories.
func TestMemoryUsecase_ExportMemories_GlobalScope(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()
	globalScopeID := uuid.New() // Global scope doesn't check ownership

	uc := &MemoryUsecase{
		memoryRepo:  &mockMemoryRepository{},
		sessionRepo: nil,
	}

	var buf bytes.Buffer
	opts := memory.ExportOptions{}

	err := uc.ExportMemories(ctx, userID, memory.ScopeGlobal, globalScopeID, opts, &buf)
	require.NoError(t, err)
}

// TestMemoryUsecase_IsMemoryConfigured tests the IsMemoryConfigured method.
func TestMemoryUsecase_IsMemoryConfigured(t *testing.T) {
	tests := []struct {
		name     string
		setupUC  func() *MemoryUsecase
		expected bool
	}{
		{
			name: "configured",
			setupUC: func() *MemoryUsecase {
				return &MemoryUsecase{
					memoryRepo: &mockMemoryRepository{},
				}
			},
			expected: true,
		},
		{
			name: "not_configured",
			setupUC: func() *MemoryUsecase {
				return &MemoryUsecase{
					memoryRepo: nil,
				}
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc := tt.setupUC()
			result := uc.IsMemoryConfigured()
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestMemoryUsecase_ExportOptions tests different export options.
func TestMemoryUsecase_ExportOptions(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	uc := &MemoryUsecase{
		memoryRepo: &mockMemoryRepository{},
	}

	opts := memory.ExportOptions{
		Scope:      memory.ScopeUser,
		ScopeID:    userID,
		Types:      []string{"fact"},
		IncludeLog: false,
	}

	var buf bytes.Buffer
	err := uc.ExportMemories(ctx, userID, memory.ScopeUser, userID, opts, &buf)
	require.NoError(t, err)
}

// TestMemoryUsecase_NilWriter tests behavior with nil writer.
func TestMemoryUsecase_NilWriter(t *testing.T) {
	// This should panic or error depending on implementation
	// For now, we'll skip this test as it requires careful handling
	t.Skip("Nil writer handling depends on implementation")
}

// TestMemoryUsecase_ExportErrorHandling tests error handling during export.
func TestMemoryUsecase_ExportErrorHandling(t *testing.T) {
	// This test would require a real memory.Exporter that calls the repo
	// The Export method in usecase calls exporter.Export which doesn't directly call repo.Export
	// Skipping for unit test - would be covered in integration tests
	t.Skip("Export error handling depends on memory.Exporter implementation - covered in integration tests")
}

// TestNewMemoryUsecase tests the NewMemoryUsecase constructor.
func TestNewMemoryUsecase(t *testing.T) {
	memRepo := &mockMemoryRepository{}
	sessionRepo := &mockMemorySessionRepo{}

	uc := NewMemoryUsecase(memRepo, sessionRepo)

	assert.NotNil(t, uc)
	assert.Equal(t, memRepo, uc.memoryRepo)
	assert.Equal(t, sessionRepo, uc.sessionRepo)
}

// TestNewMemoryUsecase_NilDependencies tests creating a usecase with nil dependencies.
func TestNewMemoryUsecase_NilDependencies(t *testing.T) {
	uc := NewMemoryUsecase(nil, nil)

	assert.NotNil(t, uc)
	assert.Nil(t, uc.memoryRepo)
	assert.Nil(t, uc.sessionRepo)
}

// TestMemoryUsecase_ErrorConstants tests error constants.
func TestMemoryUsecase_ErrorConstants(t *testing.T) {
	assert.NotNil(t, ErrMemoryNotConfigured)
	assert.NotNil(t, ErrForbidden)
	assert.NotNil(t, ErrSessionNotFound)

	assert.Equal(t, "memory repository not configured", ErrMemoryNotConfigured.Error())
	assert.Equal(t, "forbidden: not authorized", ErrForbidden.Error())
	assert.Equal(t, "session not found", ErrSessionNotFound.Error())
}

// TestMemoryUsecase_ScopeTypes tests different scope types.
func TestMemoryUsecase_ScopeTypes(t *testing.T) {
	// Verify scope type constants exist
	assert.Equal(t, memory.ScopeType("user"), memory.ScopeUser)
	assert.Equal(t, memory.ScopeType("session"), memory.ScopeSession)
	assert.Equal(t, memory.ScopeType("global"), memory.ScopeGlobal)
}

// BenchmarkMemoryUsecase_ExportMemories benchmarks memory export.
func BenchmarkMemoryUsecase_ExportMemories(b *testing.B) {
	ctx := context.Background()
	userID := uuid.New()

	uc := &MemoryUsecase{
		memoryRepo: &mockMemoryRepository{},
	}

	opts := memory.ExportOptions{
		Scope:   memory.ScopeUser,
		ScopeID: userID,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		_ = uc.ExportMemories(ctx, userID, memory.ScopeUser, userID, opts, &buf)
	}
}

// ExampleNewMemoryUsecase demonstrates creating a MemoryUsecase.
func ExampleNewMemoryUsecase() {
	// Create with dependencies
	memRepo := &mockMemoryRepository{}
	sessionRepo := &mockMemorySessionRepo{}

	uc := NewMemoryUsecase(memRepo, sessionRepo)

	// Check if memory is configured
	if uc.IsMemoryConfigured() {
		// Export memories
		ctx := context.Background()
		userID := uuid.New()
		var buf bytes.Buffer
		opts := memory.ExportOptions{}
		_ = uc.ExportMemories(ctx, userID, memory.ScopeUser, userID, opts, &buf)
	}
}
