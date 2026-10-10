package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
)

// --- Mock ServerConfigRepo ---

type mockServerConfigRepo struct {
	configs        map[string]*model.ServerConfig          // key: "key:userID"
	histories      map[string][]*model.ServerConfigHistory // key: "key:userID"
	getErr         error
	listErr        error
	upsertErr      error
	deleteErr      error
	listHistoryErr error
}

func newMockServerConfigRepo() *mockServerConfigRepo {
	return &mockServerConfigRepo{
		configs:   make(map[string]*model.ServerConfig),
		histories: make(map[string][]*model.ServerConfigHistory),
	}
}

func (m *mockServerConfigRepo) configKey(key string, userID *uuid.UUID) string {
	if userID == nil {
		return key + ":system"
	}
	return key + ":" + userID.String()
}

func (m *mockServerConfigRepo) Get(ctx context.Context, key string, userID *uuid.UUID) (*model.ServerConfig, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	k := m.configKey(key, userID)
	cfg, ok := m.configs[k]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return cfg, nil
}

func (m *mockServerConfigRepo) List(ctx context.Context, filter model.ConfigFilter) ([]*model.ServerConfig, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var result []*model.ServerConfig
	for k, cfg := range m.configs {
		// Parse the composite key to check filters
		if filter.Category != "" && cfg.Category != filter.Category {
			continue
		}
		if filter.UserID == nil {
			// System configs only (user_id IS NULL)
			if cfg.UserID == nil {
				result = append(result, cfg)
			}
		} else {
			// User configs
			if cfg.UserID != nil && *cfg.UserID == *filter.UserID {
				result = append(result, cfg)
			}
		}
		_ = k
	}
	return result, nil
}

func (m *mockServerConfigRepo) Upsert(ctx context.Context, cfg *model.ServerConfig, expectedVersion int) error {
	if m.upsertErr != nil {
		return m.upsertErr
	}
	k := m.configKey(cfg.Key, cfg.UserID)
	existing, exists := m.configs[k]

	if expectedVersion == 0 {
		// INSERT
		if exists {
			return repo.ErrConflict
		}
		if cfg.ID == uuid.Nil {
			cfg.ID = uuid.Must(uuid.NewV7())
		}
		m.configs[k] = cfg
		return nil
	}

	// UPDATE with version check
	if !exists {
		// Record doesn't exist, INSERT
		if cfg.ID == uuid.Nil {
			cfg.ID = uuid.Must(uuid.NewV7())
		}
		m.configs[k] = cfg
		return nil
	}

	if existing.Version != expectedVersion {
		return repo.ErrConflict
	}
	if cfg.ID == uuid.Nil {
		cfg.ID = existing.ID
	}
	m.configs[k] = cfg
	return nil
}

func (m *mockServerConfigRepo) Delete(ctx context.Context, key string, userID *uuid.UUID, expectedVersion int) (*model.ServerConfig, error) {
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	k := m.configKey(key, userID)
	cfg, ok := m.configs[k]
	if !ok {
		return nil, repo.ErrNotFound
	}
	if cfg.Version != expectedVersion {
		return nil, repo.ErrConflict
	}
	delete(m.configs, k)
	return cfg, nil
}

func (m *mockServerConfigRepo) ListHistory(ctx context.Context, filter model.ConfigHistoryFilter, page, pageSize int) ([]*model.ServerConfigHistory, int64, error) {
	if m.listHistoryErr != nil {
		return nil, 0, m.listHistoryErr
	}
	k := m.configKey(filter.Key, filter.UserID)
	histories := m.histories[k]

	// Paginate
	start := (page - 1) * pageSize
	if start >= len(histories) {
		return []*model.ServerConfigHistory{}, int64(len(histories)), nil
	}
	end := start + pageSize
	if end > len(histories) {
		end = len(histories)
	}
	return histories[start:end], int64(len(histories)), nil
}

func (m *mockServerConfigRepo) GetHistoryByVersion(ctx context.Context, key string, userID *uuid.UUID, version int) (*model.ServerConfigHistory, error) {
	k := m.configKey(key, userID)
	histories := m.histories[k]
	for _, h := range histories {
		if h.Version == version {
			return h, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (m *mockServerConfigRepo) TrimHistory(ctx context.Context, key string, userID *uuid.UUID, maxCount int) error {
	k := m.configKey(key, userID)
	histories := m.histories[k]
	if len(histories) <= maxCount {
		return nil
	}
	m.histories[k] = histories[len(histories)-maxCount:]
	return nil
}

func (m *mockServerConfigRepo) DeleteByUserID(ctx context.Context, userID uuid.UUID) ([]*model.ServerConfig, error) {
	var deleted []*model.ServerConfig
	for k, cfg := range m.configs {
		if cfg.UserID != nil && *cfg.UserID == userID {
			deleted = append(deleted, cfg)
			delete(m.configs, k)
		}
	}
	return deleted, nil
}

// --- Mock AuditLogRepo ---

type mockAuditLogRepo struct {
	logs    []*model.AuditLog
	createF func(ctx context.Context, log *model.AuditLog) error
}

func newMockAuditLogRepo() *mockAuditLogRepo {
	return &mockAuditLogRepo{}
}

func (m *mockAuditLogRepo) Create(ctx context.Context, log *model.AuditLog) error {
	if m.createF != nil {
		return m.createF(ctx, log)
	}
	m.logs = append(m.logs, log)
	return nil
}

func (m *mockAuditLogRepo) Get(ctx context.Context, id uuid.UUID) (*model.AuditLog, error) {
	return nil, nil
}

func (m *mockAuditLogRepo) List(ctx context.Context, filter repo.AuditLogFilter, page, pageSize int) ([]*model.AuditLog, int64, error) {
	return nil, 0, nil
}

// --- Mock OAuth2UserRepo ---

type mockOAuth2UserRepoForConfig struct {
	users map[uuid.UUID]*model.OAuth2User
}

func newMockOAuth2UserRepoForConfig() *mockOAuth2UserRepoForConfig {
	return &mockOAuth2UserRepoForConfig{
		users: make(map[uuid.UUID]*model.OAuth2User),
	}
}

func (m *mockOAuth2UserRepoForConfig) FindByID(ctx context.Context, id uuid.UUID) (*model.OAuth2User, error) {
	user, ok := m.users[id]
	if !ok {
		return nil, repo.ErrOAuth2UserNotFound
	}
	return user, nil
}

// Implement other required methods for OAuth2UserRepo interface
func (m *mockOAuth2UserRepoForConfig) Create(ctx context.Context, user *model.OAuth2User) error {
	return nil
}

func (m *mockOAuth2UserRepoForConfig) FindByProvider(ctx context.Context, provider, sub string) (*model.OAuth2User, error) {
	return nil, nil
}

func (m *mockOAuth2UserRepoForConfig) FindOrCreate(ctx context.Context, user *model.OAuth2User) (*model.OAuth2User, bool, error) {
	return nil, false, nil
}

func (m *mockOAuth2UserRepoForConfig) Update(ctx context.Context, user *model.OAuth2User) error {
	return nil
}

func (m *mockOAuth2UserRepoForConfig) ListWithFilters(ctx context.Context, filter repo.OAuth2UserFilter) ([]*model.OAuth2User, int64, error) {
	return nil, 0, nil
}

func (m *mockOAuth2UserRepoForConfig) IsUserBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}

// --- Test Helpers ---

// newTestUsecase creates a usecase with mock dependencies.
// Note: We cannot easily mock gorm.DB, so we skip testing methods that require
// real transactions (UpdateSystemConfig, SetUserConfigOverride, DeleteSystemConfig,
// DeleteUserConfigOverride, RollbackConfig, BootstrapDynamicConfigs).
// Instead, we focus on testing the business logic that doesn't require DB transactions.
func newTestUsecase(
	configRepo *mockServerConfigRepo,
	auditRepo *mockAuditLogRepo,
	userRepo *mockOAuth2UserRepoForConfig,
) *usecase.ServerConfigUsecase {
	// Note: db is nil, which means transaction-based methods will panic.
	// We'll test those separately with a different approach.
	return usecase.NewServerConfigUsecase(configRepo, auditRepo, userRepo, nil)
}

// --- Tests for GetEffectiveValue ---

func TestGetEffectiveValue(t *testing.T) {
	ctx := context.Background()

	testKey := "llm.model"
	testYamlDefault := "claude-sonnet-4-5"

	// Ensure the registry entry exists
	entry := config.GetRegistryEntry(testKey)
	require.NotNil(t, entry, "test key must exist in registry")
	entry.YamlDefault = testYamlDefault

	t.Run("returns yaml default when no DB records", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		result, err := uc.GetEffectiveValue(ctx, testKey, nil)
		require.NoError(t, err)
		assert.Equal(t, testYamlDefault, result)
	})

	t.Run("returns system value when user has no override", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add system config
		systemValue := datatypes.JSON([]byte(`"claude-3-opus"`))
		configRepo.configs[configRepo.configKey(testKey, nil)] = &model.ServerConfig{
			Key:     testKey,
			UserID:  nil,
			Value:   systemValue,
			Version: 1,
		}

		userID := uuid.Must(uuid.NewV7())
		result, err := uc.GetEffectiveValue(ctx, testKey, &userID)
		require.NoError(t, err)
		assert.Equal(t, "claude-3-opus", result)
	})

	t.Run("returns user override when exists", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add system config
		systemValue := datatypes.JSON([]byte(`"claude-3-opus"`))
		configRepo.configs[configRepo.configKey(testKey, nil)] = &model.ServerConfig{
			Key:     testKey,
			UserID:  nil,
			Value:   systemValue,
			Version: 1,
		}

		// Add user override
		userID := uuid.Must(uuid.NewV7())
		userValue := datatypes.JSON([]byte(`"gpt-4"`))
		configRepo.configs[configRepo.configKey(testKey, &userID)] = &model.ServerConfig{
			Key:     testKey,
			UserID:  &userID,
			Value:   userValue,
			Version: 1,
		}

		result, err := uc.GetEffectiveValue(ctx, testKey, &userID)
		require.NoError(t, err)
		assert.Equal(t, "gpt-4", result)
	})

	t.Run("returns error for unknown config key", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		_, err := uc.GetEffectiveValue(ctx, "unknown.key", nil)
		assert.ErrorIs(t, err, usecase.ErrConfigKeyNotFound)
	})

	t.Run("returns error when DB fails", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		configRepo.getErr = errors.New("database error")
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		_, err := uc.GetEffectiveValue(ctx, testKey, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "get system config")
	})
}

// --- Tests for GetSystemConfig ---

func TestGetSystemConfig(t *testing.T) {
	ctx := context.Background()

	testKey := "llm.max_tokens"
	testYamlDefault := 16000

	// Ensure the registry entry exists
	entry := config.GetRegistryEntry(testKey)
	require.NotNil(t, entry, "test key must exist in registry")
	entry.YamlDefault = testYamlDefault

	t.Run("returns yaml default when no DB record", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		result, err := uc.GetSystemConfig(ctx, testKey)
		require.NoError(t, err)
		assert.Equal(t, testKey, result.Key)
		assert.Equal(t, "yaml", result.Source)
		assert.Equal(t, testYamlDefault, result.YamlDefault)
		assert.Equal(t, 0, result.Version)
	})

	t.Run("returns system value when exists", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add system config
		systemValue := datatypes.JSON([]byte(`32000`))
		updatedBy := uuid.Must(uuid.NewV7())
		now := time.Now()
		configRepo.configs[configRepo.configKey(testKey, nil)] = &model.ServerConfig{
			Key:       testKey,
			UserID:    nil,
			Value:     systemValue,
			ValueType: entry.ValueType,
			Category:  entry.Category,
			Version:   5,
			UpdatedBy: &updatedBy,
			UpdatedAt: now,
		}

		result, err := uc.GetSystemConfig(ctx, testKey)
		require.NoError(t, err)
		assert.Equal(t, testKey, result.Key)
		assert.Equal(t, "system", result.Source)
		assert.Equal(t, float64(32000), result.Value) // JSON unmarshals to float64
		assert.Equal(t, 5, result.Version)
		assert.NotNil(t, result.UpdatedBy)
		assert.NotNil(t, result.UpdatedAt)
	})

	t.Run("returns error for unknown config key", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		_, err := uc.GetSystemConfig(ctx, "unknown.key")
		assert.ErrorIs(t, err, usecase.ErrConfigKeyNotFound)
	})
}

// --- Tests for ListSystemConfigs ---

func TestListSystemConfigs(t *testing.T) {
	ctx := context.Background()

	t.Run("returns all configs with pagination", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Get total count of registry keys
		totalKeys := len(config.AllRegistryKeys())

		result, total, err := uc.ListSystemConfigs(ctx, "", 1, 10)
		require.NoError(t, err)
		assert.Equal(t, totalKeys, total)
		assert.Len(t, result, 10) // First page
	})

	t.Run("filters by category", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		result, total, err := uc.ListSystemConfigs(ctx, config.CategoryLLM, 1, 100)
		require.NoError(t, err)
		assert.Greater(t, total, 0)
		for _, item := range result {
			assert.Equal(t, config.CategoryLLM, item.Category)
		}
	})

	t.Run("returns empty page when out of range", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		result, total, err := uc.ListSystemConfigs(ctx, "", 9999, 10)
		require.NoError(t, err)
		assert.Greater(t, total, 0)
		assert.Len(t, result, 0)
	})

	t.Run("returns error when repo fails", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		configRepo.listErr = errors.New("database error")
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		_, _, err := uc.ListSystemConfigs(ctx, "", 1, 10)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "list system configs")
	})
}

// --- Tests for GetUserConfig ---

func TestGetUserConfig(t *testing.T) {
	ctx := context.Background()

	testKey := "llm.model"
	testYamlDefault := "claude-sonnet-4-5"

	// Ensure the registry entry exists
	entry := config.GetRegistryEntry(testKey)
	require.NotNil(t, entry, "test key must exist in registry")
	entry.YamlDefault = testYamlDefault

	t.Run("returns yaml default when no overrides", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		userID := uuid.Must(uuid.NewV7())
		result, err := uc.GetUserConfig(ctx, userID, testKey)
		require.NoError(t, err)
		assert.Equal(t, testKey, result.Key)
		assert.Equal(t, "yaml", result.Source)
		assert.Equal(t, testYamlDefault, result.EffectiveValue)
	})

	t.Run("returns system value when no user override", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add system config
		systemValue := datatypes.JSON([]byte(`"claude-3-opus"`))
		configRepo.configs[configRepo.configKey(testKey, nil)] = &model.ServerConfig{
			Key:     testKey,
			UserID:  nil,
			Value:   systemValue,
			Version: 1,
		}

		userID := uuid.Must(uuid.NewV7())
		result, err := uc.GetUserConfig(ctx, userID, testKey)
		require.NoError(t, err)
		assert.Equal(t, "system", result.Source)
		assert.Equal(t, "claude-3-opus", result.EffectiveValue)
		assert.Equal(t, "claude-3-opus", result.SystemValue)
	})

	t.Run("returns user override when exists", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add system config
		systemValue := datatypes.JSON([]byte(`"claude-3-opus"`))
		configRepo.configs[configRepo.configKey(testKey, nil)] = &model.ServerConfig{
			Key:     testKey,
			UserID:  nil,
			Value:   systemValue,
			Version: 1,
		}

		// Add user override
		userID := uuid.Must(uuid.NewV7())
		userValue := datatypes.JSON([]byte(`"gpt-4"`))
		now := time.Now()
		configRepo.configs[configRepo.configKey(testKey, &userID)] = &model.ServerConfig{
			Key:       testKey,
			UserID:    &userID,
			Value:     userValue,
			Version:   2,
			UpdatedAt: now,
		}

		result, err := uc.GetUserConfig(ctx, userID, testKey)
		require.NoError(t, err)
		assert.Equal(t, "user", result.Source)
		assert.Equal(t, "gpt-4", result.EffectiveValue)
		assert.Equal(t, "gpt-4", result.UserValue)
		assert.Equal(t, "claude-3-opus", result.SystemValue)
		assert.Equal(t, 2, result.Version)
	})

	t.Run("returns error for unknown config key", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		userID := uuid.Must(uuid.NewV7())
		_, err := uc.GetUserConfig(ctx, userID, "unknown.key")
		assert.ErrorIs(t, err, usecase.ErrConfigKeyNotFound)
	})
}

// --- Tests for GetUserConfigsView ---

func TestGetUserConfigsView(t *testing.T) {
	ctx := context.Background()

	t.Run("returns merged view with correct priority", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		userID := uuid.Must(uuid.NewV7())

		// Add system config for llm.model
		systemValue := datatypes.JSON([]byte(`"claude-3-opus"`))
		configRepo.configs[configRepo.configKey("llm.model", nil)] = &model.ServerConfig{
			Key:      "llm.model",
			UserID:   nil,
			Value:    systemValue,
			Category: config.CategoryLLM,
			Version:  1,
		}

		// Add user override for llm.model
		userValue := datatypes.JSON([]byte(`"gpt-4"`))
		configRepo.configs[configRepo.configKey("llm.model", &userID)] = &model.ServerConfig{
			Key:       "llm.model",
			UserID:    &userID,
			Value:     userValue,
			Category:  config.CategoryLLM,
			Version:   2,
			UpdatedAt: time.Now(),
		}

		result, err := uc.GetUserConfigsView(ctx, userID, config.CategoryLLM)
		require.NoError(t, err)
		assert.Greater(t, len(result), 0)

		// Find the llm.model item
		var llmModelItem *usecase.UserConfigItem
		for _, item := range result {
			if item.Key == "llm.model" {
				llmModelItem = &item
				break
			}
		}
		require.NotNil(t, llmModelItem)
		assert.Equal(t, "user", llmModelItem.Source)
		assert.Equal(t, "gpt-4", llmModelItem.EffectiveValue)
		assert.Equal(t, "claude-3-opus", llmModelItem.SystemValue)
		assert.Equal(t, "gpt-4", llmModelItem.UserValue)
	})

	t.Run("filters by category", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		userID := uuid.Must(uuid.NewV7())
		result, err := uc.GetUserConfigsView(ctx, userID, config.CategoryWorker)
		require.NoError(t, err)
		for _, item := range result {
			assert.Equal(t, config.CategoryWorker, item.Category)
		}
	})

	t.Run("returns error when repo fails", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		configRepo.listErr = errors.New("database error")
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		userID := uuid.Must(uuid.NewV7())
		_, err := uc.GetUserConfigsView(ctx, userID, "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "list system configs")
	})
}

// --- Tests for GetHistory ---

func TestGetHistory(t *testing.T) {
	ctx := context.Background()

	testKey := "llm.model"

	t.Run("returns history records", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add history records
		changedBy := uuid.Must(uuid.NewV7())
		now := time.Now()
		configRepo.histories[configRepo.configKey(testKey, nil)] = []*model.ServerConfigHistory{
			{
				Key:       testKey,
				Version:   1,
				OldValue:  nil,
				NewValue:  datatypes.JSON([]byte(`"claude-3-opus"`)),
				ChangedBy: changedBy,
				ChangedAt: now.Add(-2 * time.Hour),
			},
			{
				Key:       testKey,
				Version:   2,
				OldValue:  datatypes.JSON([]byte(`"claude-3-opus"`)),
				NewValue:  datatypes.JSON([]byte(`"gpt-4"`)),
				ChangedBy: changedBy,
				ChangedAt: now.Add(-1 * time.Hour),
			},
		}

		result, total, err := uc.GetHistory(ctx, testKey, nil, 1, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(2), total)
		assert.Len(t, result, 2)
		assert.Equal(t, 1, result[0].Version)
		assert.Equal(t, 2, result[1].Version)
	})

	t.Run("returns empty list when no history", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		result, total, err := uc.GetHistory(ctx, testKey, nil, 1, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(0), total)
		assert.Len(t, result, 0)
	})

	t.Run("returns error for unknown config key", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		_, _, err := uc.GetHistory(ctx, "unknown.key", nil, 1, 10)
		assert.ErrorIs(t, err, usecase.ErrConfigKeyNotFound)
	})

	t.Run("returns error when repo fails", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		configRepo.listHistoryErr = errors.New("database error")
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		_, _, err := uc.GetHistory(ctx, testKey, nil, 1, 10)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "list config history")
	})

	t.Run("paginates history", func(t *testing.T) {
		configRepo := newMockServerConfigRepo()
		auditRepo := newMockAuditLogRepo()
		userRepo := newMockOAuth2UserRepoForConfig()
		uc := newTestUsecase(configRepo, auditRepo, userRepo)

		// Add 5 history records
		changedBy := uuid.Must(uuid.NewV7())
		now := time.Now()
		var histories []*model.ServerConfigHistory
		for i := 1; i <= 5; i++ {
			histories = append(histories, &model.ServerConfigHistory{
				Key:       testKey,
				Version:   i,
				NewValue:  datatypes.JSON([]byte(`"value"`)),
				ChangedBy: changedBy,
				ChangedAt: now.Add(time.Duration(i) * time.Hour),
			})
		}
		configRepo.histories[configRepo.configKey(testKey, nil)] = histories

		// First page
		result1, total1, err1 := uc.GetHistory(ctx, testKey, nil, 1, 2)
		require.NoError(t, err1)
		assert.Equal(t, int64(5), total1)
		assert.Len(t, result1, 2)

		// Second page
		result2, total2, err2 := uc.GetHistory(ctx, testKey, nil, 2, 2)
		require.NoError(t, err2)
		assert.Equal(t, int64(5), total2)
		assert.Len(t, result2, 2)

		// Third page (partial)
		result3, total3, err3 := uc.GetHistory(ctx, testKey, nil, 3, 2)
		require.NoError(t, err3)
		assert.Equal(t, int64(5), total3)
		assert.Len(t, result3, 1)
	})
}

// --- Tests for SetConfigPublisher ---

func TestSetConfigPublisher(t *testing.T) {

	configRepo := newMockServerConfigRepo()
	auditRepo := newMockAuditLogRepo()
	userRepo := newMockOAuth2UserRepoForConfig()
	uc := newTestUsecase(configRepo, auditRepo, userRepo)

	// Create a mock publisher
	mockPublisher := &mockConfigPublisher{}

	// Should not panic
	uc.SetConfigPublisher(mockPublisher)

	// Should be able to set to nil
	uc.SetConfigPublisher(nil)
}

type mockConfigPublisher struct {
	publishErr error
	published  []struct {
		key       string
		userID    *uuid.UUID
		version   int
		operation string
	}
}

func (m *mockConfigPublisher) PublishConfigUpdate(ctx context.Context, key string, userID *uuid.UUID, version int, operation string) error {
	if m.publishErr != nil {
		return m.publishErr
	}
	m.published = append(m.published, struct {
		key       string
		userID    *uuid.UUID
		version   int
		operation string
	}{key, userID, version, operation})
	return nil
}
