package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
)

// mockDeps holds mock dependencies for testing transaction-based methods.
type mockDeps struct {
	db         *gorm.DB
	mock       sqlmock.Sqlmock
	configRepo repo.ServerConfigRepo
	auditRepo  *mockAuditLogRepoForTx
	userRepo   *mockOAuth2UserRepoForTx
	uc         *ServerConfigUsecase
}

type mockAuditLogRepoForTx struct {
	logs []*model.AuditLog
}

func (m *mockAuditLogRepoForTx) Create(ctx context.Context, log *model.AuditLog) error {
	m.logs = append(m.logs, log)
	return nil
}

func (m *mockAuditLogRepoForTx) Get(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
	return nil, nil
}

func (m *mockAuditLogRepoForTx) List(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error) {
	return nil, 0, nil
}

type mockOAuth2UserRepoForTx struct {
	users map[uuid.UUID]*model.OAuth2User
}

func (m *mockOAuth2UserRepoForTx) FindByID(ctx context.Context, id uuid.UUID) (*model.OAuth2User, error) {
	user, ok := m.users[id]
	if !ok {
		return nil, repo.ErrOAuth2UserNotFound
	}
	return user, nil
}

func (m *mockOAuth2UserRepoForTx) Create(ctx context.Context, user *model.OAuth2User) error {
	return nil
}

func (m *mockOAuth2UserRepoForTx) FindByProvider(ctx context.Context, provider, sub string) (*model.OAuth2User, error) {
	return nil, nil
}

func (m *mockOAuth2UserRepoForTx) FindOrCreate(ctx context.Context, user *model.OAuth2User) (*model.OAuth2User, bool, error) {
	return nil, false, nil
}

func (m *mockOAuth2UserRepoForTx) Update(ctx context.Context, user *model.OAuth2User) error {
	return nil
}

func (m *mockOAuth2UserRepoForTx) ListWithFilters(ctx context.Context, filter repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	return nil, 0, nil
}

func (m *mockOAuth2UserRepoForTx) IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}

// setupMockDB creates a mock database and returns all necessary dependencies.
func setupMockDB(t *testing.T) *mockDeps {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	// Create gorm.DB with postgres driver
	gormDB, err := gorm.Open(postgres.New(postgres.Config{
		Conn:       db,
		DriverName: "postgres",
	}), &gorm.Config{})
	require.NoError(t, err)

	configRepo := repo.NewServerConfigRepo(gormDB)
	auditRepo := &mockAuditLogRepoForTx{}
	userRepo := &mockOAuth2UserRepoForTx{users: make(map[uuid.UUID]*model.OAuth2User)}

	uc := NewServerConfigUsecase(configRepo, auditRepo, userRepo, gormDB)

	return &mockDeps{
		db:         gormDB,
		mock:       mock,
		configRepo: configRepo,
		auditRepo:  auditRepo,
		userRepo:   userRepo,
		uc:         uc,
	}
}

// TestUpdateSystemConfig_InvalidKey tests that UpdateSystemConfig returns error for unknown key.
func TestUpdateSystemConfig_InvalidKey(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "unknown.key"
	input := UpdateConfigInput{Value: "test", Version: 0}
	operatorID := uuid.Must(uuid.NewV7())

	_, err := deps.uc.UpdateSystemConfig(ctx, key, input, operatorID, "127.0.0.1")
	assert.ErrorIs(t, err, ErrConfigKeyNotFound)
}

// TestUpdateSystemConfig_ValidationError tests validation errors.
func TestUpdateSystemConfig_ValidationError(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "llm.max_tokens"                                       // int type
	input := UpdateConfigInput{Value: "not a number", Version: 0} // invalid type
	operatorID := uuid.Must(uuid.NewV7())

	_, err := deps.uc.UpdateSystemConfig(ctx, key, input, operatorID, "127.0.0.1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")
}

// TestSetUserConfigOverride_InvalidKey tests that SetUserConfigOverride returns error for unknown key.
func TestSetUserConfigOverride_InvalidKey(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "unknown.key"
	userID := uuid.Must(uuid.NewV7())
	input := UpdateConfigInput{Value: "test", Version: 0}
	operatorID := uuid.Must(uuid.NewV7())

	_, err := deps.uc.SetUserConfigOverride(ctx, userID, key, input, operatorID, "127.0.0.1")
	assert.ErrorIs(t, err, ErrConfigKeyNotFound)
}

// TestSetUserConfigOverride_UserNotFound tests that SetUserConfigOverride returns error when user doesn't exist.
func TestSetUserConfigOverride_UserNotFound(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "llm.model"
	userID := uuid.Must(uuid.NewV7())
	input := UpdateConfigInput{Value: "test", Version: 0}
	operatorID := uuid.Must(uuid.NewV7())

	_, err := deps.uc.SetUserConfigOverride(ctx, userID, key, input, operatorID, "127.0.0.1")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

// TestDeleteSystemConfig_InvalidKey tests that DeleteSystemConfig returns error for unknown key.
func TestDeleteSystemConfig_InvalidKey(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "unknown.key"
	operatorID := uuid.Must(uuid.NewV7())

	_, _, err := deps.uc.DeleteSystemConfig(ctx, key, 1, operatorID, "127.0.0.1")
	assert.ErrorIs(t, err, ErrConfigKeyNotFound)
}

// TestDeleteUserConfigOverride_InvalidKey tests that DeleteUserConfigOverride returns error for unknown key.
func TestDeleteUserConfigOverride_InvalidKey(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "unknown.key"
	userID := uuid.Must(uuid.NewV7())
	operatorID := uuid.Must(uuid.NewV7())

	_, _, err := deps.uc.DeleteUserConfigOverride(ctx, userID, key, 1, operatorID, "127.0.0.1")
	assert.ErrorIs(t, err, ErrConfigKeyNotFound)
}

// TestRollbackConfig_InvalidKey tests that RollbackConfig returns error for unknown key.
func TestRollbackConfig_InvalidKey(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "unknown.key"
	input := RollbackConfigInput{TargetVersion: 1, Version: 2}
	operatorID := uuid.Must(uuid.NewV7())

	_, _, err := deps.uc.RollbackConfig(ctx, key, nil, input, operatorID, "127.0.0.1")
	assert.ErrorIs(t, err, ErrConfigKeyNotFound)
}

// TestGetOldValue tests the getOldValue helper function.
func TestGetOldValue(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "llm.model"
	entry := config.GetRegistryEntry(key)
	require.NotNil(t, entry)

	// Test when config doesn't exist (should return yaml default)
	deps.mock.ExpectQuery(`SELECT \* FROM "server_configs" WHERE key = \$1 AND user_id IS NULL`).
		WillReturnError(gorm.ErrRecordNotFound)

	result := deps.uc.getOldValue(ctx, key, nil, entry)
	// Should return yaml default as JSON
	assert.NotNil(t, result)
}

// TestUpdateSystemConfig_Success tests successful update flow.
func TestUpdateSystemConfig_Success(t *testing.T) {
	deps := setupMockDB(t)
	defer deps.mock.ExpectClose()

	ctx := context.Background()
	key := "llm.model"
	entry := config.GetRegistryEntry(key)
	require.NotNil(t, entry)

	input := UpdateConfigInput{Value: "gpt-4", Version: 0, ChangeNote: "test update"}
	operatorID := uuid.Must(uuid.NewV7())

	// Mock transaction
	deps.mock.ExpectBegin()

	// Mock SELECT for getOldValue (config doesn't exist)
	deps.mock.ExpectQuery(`SELECT \* FROM "server_configs" WHERE key = \$1 AND user_id IS NULL`).
		WillReturnError(gorm.ErrRecordNotFound)

	// Mock INSERT for Upsert (version=0 means INSERT)
	deps.mock.ExpectExec(`INSERT INTO "server_configs"`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Mock INSERT for history
	deps.mock.ExpectExec(`INSERT INTO "server_config_history"`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	deps.mock.ExpectCommit()

	// Mock SELECT for GetSystemConfig after update
	rows := sqlmock.NewRows([]string{"id", "key", "user_id", "value", "value_type", "category", "description", "version", "updated_by", "updated_at", "created_at"}).
		AddRow(uuid.Must(uuid.NewV7()), key, nil, `"gpt-4"`, entry.ValueType, entry.Category, entry.Description, 1, operatorID, time.Now(), time.Now())
	deps.mock.ExpectQuery(`SELECT \* FROM "server_configs" WHERE key = \$1 AND user_id IS NULL`).
		WillReturnRows(rows)

	// Mock TrimHistory (should not fail the operation)
	deps.mock.ExpectBegin()
	deps.mock.ExpectQuery(`SELECT count\(\*\) FROM "server_config_history" WHERE key = \$1 AND user_id IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	deps.mock.ExpectCommit()

	// Mock audit log create (should not fail the operation)
	deps.mock.ExpectExec(`INSERT INTO "admin_audit_logs"`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	result, err := deps.uc.UpdateSystemConfig(ctx, key, input, operatorID, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, key, result.Key)
	assert.Equal(t, "system", result.Source)
}
