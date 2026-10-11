package usecase

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// mockAuditLogRepo is a mock implementation of repo.AuditLogRepo for testing.
type mockAuditLogRepo struct {
	listFunc func(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error)
}

func (m *mockAuditLogRepo) Create(ctx context.Context, log *model.AuditLog) error {
	return nil
}

func (m *mockAuditLogRepo) Get(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
	return nil, nil
}

func (m *mockAuditLogRepo) List(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, filter, page, pageSize)
	}
	return nil, 0, nil
}

// TestAuditLogList_PaginationValidation tests that pagination parameters are validated.
func TestAuditLogList_PaginationValidation(t *testing.T) {
	uc := NewAuditLogUsecase(&mockAuditLogRepo{})
	ctx := context.Background()

	tests := []struct {
		name         string
		page         int
		pageSize     int
		expectError  bool
		expectedPage int
		expectedSize int
	}{
		{
			name:         "valid_pagination",
			page:         2,
			pageSize:     20,
			expectError:  false,
			expectedPage: 2,
			expectedSize: 20,
		},
		{
			name:         "page_zero_normalized_to_one",
			page:         0,
			pageSize:     20,
			expectError:  false,
			expectedPage: 1,
			expectedSize: 20,
		},
		{
			name:         "negative_page_normalized_to_one",
			page:         -5,
			pageSize:     20,
			expectError:  false,
			expectedPage: 1,
			expectedSize: 20,
		},
		{
			name:         "zero_pagesize_normalized_to_20",
			page:         1,
			pageSize:     0,
			expectError:  false,
			expectedPage: 1,
			expectedSize: 20,
		},
		{
			name:         "negative_pagesize_normalized_to_20",
			page:         1,
			pageSize:     -10,
			expectError:  false,
			expectedPage: 1,
			expectedSize: 20,
		},
		{
			name:        "pagesize_exceeds_maximum",
			page:        1,
			pageSize:    101,
			expectError: true,
		},
		{
			name:         "pagesize_at_boundary_100",
			page:         1,
			pageSize:     100,
			expectError:  false,
			expectedPage: 1,
			expectedSize: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedPage, receivedSize int
			mockRepo := &mockAuditLogRepo{
				listFunc: func(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error) {
					receivedPage = page
					receivedSize = pageSize
					return []*model.AuditLog{}, 0, nil
				},
			}
			uc = NewAuditLogUsecase(mockRepo)

			result, err := uc.List(ctx, AuditLogFilter{}, tt.page, tt.pageSize)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "exceeds maximum 100")
				assert.Nil(t, result)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, result)
				assert.Equal(t, tt.expectedPage, receivedPage, "page should be normalized")
				assert.Equal(t, tt.expectedSize, receivedSize, "pageSize should be normalized")
			}
		})
	}
}
