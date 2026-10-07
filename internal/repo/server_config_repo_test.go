package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

// TestConfigRepo_NullUserID_Integration tests that system configs (NULL user_id) work correctly.
// This is an integration test that requires a real PostgreSQL database.
func TestConfigRepo_NullUserID_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	// This test requires a real database connection.
	// Set DATABASE_URL environment variable to run this test.
	dsn := "postgres://postgres:postgres@localhost:5432/rtc_agent_test?sslmode=disable"
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("Cannot connect to test database: %v", err)
	}

	// Auto-migrate
	err = db.AutoMigrate(&model.ServerConfig{}, &model.ServerConfigHistory{})
	require.NoError(t, err)

	// Create partial unique indexes
	err = db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_server_configs_key_system
		ON server_configs(key)
		WHERE user_id IS NULL
	`).Error
	require.NoError(t, err)

	err = db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_server_configs_key_user
		ON server_configs(key, user_id)
		WHERE user_id IS NOT NULL
	`).Error
	require.NoError(t, err)

	repo := NewServerConfigRepo(db)
	ctx := context.Background()

	// Clean up before test
	db.Where("key = ?", "test.config").Delete(&model.ServerConfig{})

	t.Run("Create system config with NULL user_id", func(t *testing.T) {
		cfg := &model.ServerConfig{
			Key:         "test.config",
			UserID:      nil, // System config
			Value:       datatypes.JSON(`"test_value"`),
			ValueType:   "string",
			Category:    "test",
			Description: "Test config",
			Version:     1,
		}

		err := repo.Upsert(ctx, cfg, 0)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, cfg.ID, "ID should be generated")

		// Retrieve and verify
		retrieved, err := repo.Get(ctx, "test.config", nil)
		require.NoError(t, err)
		assert.Equal(t, cfg.ID, retrieved.ID)
		assert.Nil(t, retrieved.UserID)
		assert.Equal(t, "test.config", retrieved.Key)
	})

	t.Run("Prevent duplicate system config for same key", func(t *testing.T) {
		cfg2 := &model.ServerConfig{
			Key:         "test.config",
			UserID:      nil, // Same key, NULL user_id
			Value:       datatypes.JSON(`"another_value"`),
			ValueType:   "string",
			Category:    "test",
			Description: "Duplicate config",
			Version:     1,
		}

		err := repo.Upsert(ctx, cfg2, 0)
		assert.Error(t, err, "Should fail due to unique constraint")
		assert.True(t, IsDuplicateKeyError(err), "Should be duplicate key error")
	})

	t.Run("Create user config with non-NULL user_id", func(t *testing.T) {
		userID := uuid.New()
		cfg := &model.ServerConfig{
			Key:         "test.config",
			UserID:      &userID, // User override
			Value:       datatypes.JSON(`"user_value"`),
			ValueType:   "string",
			Category:    "test",
			Description: "User config",
			Version:     1,
		}

		err := repo.Upsert(ctx, cfg, 0)
		require.NoError(t, err)

		// Retrieve and verify
		retrieved, err := repo.Get(ctx, "test.config", &userID)
		require.NoError(t, err)
		assert.Equal(t, cfg.ID, retrieved.ID)
		assert.NotNil(t, retrieved.UserID)
		assert.Equal(t, userID, *retrieved.UserID)
	})

	t.Run("List system configs only", func(t *testing.T) {
		configs, err := repo.List(ctx, model.ConfigFilter{UserID: nil})
		require.NoError(t, err)

		// Should only return system configs (NULL user_id)
		for _, cfg := range configs {
			assert.Nil(t, cfg.UserID, "System config should have NULL user_id")
		}
	})

	t.Run("List user configs only", func(t *testing.T) {
		userID := uuid.New()
		configs, err := repo.List(ctx, model.ConfigFilter{UserID: &userID})
		require.NoError(t, err)

		// Should only return configs for this user
		for _, cfg := range configs {
			assert.NotNil(t, cfg.UserID, "User config should have non-NULL user_id")
			assert.Equal(t, userID, *cfg.UserID)
		}
	})

	// Clean up
	db.Where("key = ?", "test.config").Delete(&model.ServerConfig{})
}

// TestServerConfig_Model_Validation tests model-level constraints.
func TestServerConfig_Model_Validation(t *testing.T) {
	t.Run("UserID pointer allows nil", func(t *testing.T) {
		cfg := &model.ServerConfig{
			Key:    "test.key",
			UserID: nil, // System config
		}
		assert.Nil(t, cfg.UserID)
	})

	t.Run("UserID pointer allows non-nil", func(t *testing.T) {
		userID := uuid.New()
		cfg := &model.ServerConfig{
			Key:    "test.key",
			UserID: &userID,
		}
		assert.NotNil(t, cfg.UserID)
		assert.Equal(t, userID, *cfg.UserID)
	})

	t.Run("BeforeCreate generates UUID", func(t *testing.T) {
		cfg := &model.ServerConfig{
			Key:    "test.key",
			UserID: nil,
		}
		assert.Equal(t, uuid.Nil, cfg.ID)

		err := cfg.BeforeCreate(nil)
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, cfg.ID)
	})

	t.Run("BeforeCreate preserves existing ID", func(t *testing.T) {
		existingID := uuid.New()
		cfg := &model.ServerConfig{
			ID:     existingID,
			Key:    "test.key",
			UserID: nil,
		}

		err := cfg.BeforeCreate(nil)
		require.NoError(t, err)
		assert.Equal(t, existingID, cfg.ID)
	})
}
