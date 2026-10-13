package usecase

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// mockAuditLogRepo is a mock implementation of repo.AuditLogRepo for testing.
type mockAuditLogRepo struct {
	listFunc func(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error)
	getFunc  func(ctx context.Context, id uuid.UUID) (*model.AuditLog, error)
}

func (m *mockAuditLogRepo) Create(ctx context.Context, log *model.AuditLog) error {
	return nil
}

func (m *mockAuditLogRepo) Get(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, id)
	}
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

// TestAuditLogGet tests the Get method.
func TestAuditLogGet(t *testing.T) {
	ctx := context.Background()
	logID := uuid.New()

	tests := []struct {
		name          string
		setupMock     func() *mockAuditLogRepo
		expectError   bool
		errorContains string
	}{
		{
			name: "success",
			setupMock: func() *mockAuditLogRepo {
				return &mockAuditLogRepo{
					getFunc: func(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
						return &model.AuditLog{
							ID:           id,
							OperatorID:   uuid.New(),
							OperatorIP:   "127.0.0.1",
							EventType:    "create_user",
							ResourceType: "user",
							ResourceID:   uuid.New(),
							Details:      []byte(`{"test": "data"}`),
							CreatedAt:    time.Now(),
						}, nil
					},
				}
			},
			expectError: false,
		},
		{
			name: "not_found",
			setupMock: func() *mockAuditLogRepo {
				return &mockAuditLogRepo{
					getFunc: func(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
						return nil, sql.ErrNoRows
					},
				}
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := tt.setupMock()
			uc := NewAuditLogUsecase(mockRepo)

			result, err := uc.Get(ctx, logID)

			if tt.expectError {
				require.Error(t, err)
				assert.Nil(t, result)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, result)
				assert.Equal(t, logID, result.ID)
			}
		})
	}
}

// TestAuditLogList_TimeRangeValidation tests time range validation.
func TestAuditLogList_TimeRangeValidation(t *testing.T) {
	ctx := context.Background()

	mockRepo := &mockAuditLogRepo{
		listFunc: func(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error) {
			return []*model.AuditLog{}, 0, nil
		},
	}
	uc := NewAuditLogUsecase(mockRepo)

	// Test invalid time range (startTime after endTime)
	startTime := time.Now()
	endTime := startTime.Add(-1 * time.Hour) // endTime before startTime

	filter := AuditLogFilter{
		StartTime: &startTime,
		EndTime:   &endTime,
	}

	result, err := uc.List(ctx, filter, 1, 20)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startTime must be before or equal to endTime")
	assert.Nil(t, result)
}
