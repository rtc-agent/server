package repo

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// ─── TrimHistory Tests ───

// setupConfigHistoryTestDB creates a test database with required models and indexes.
func setupConfigHistoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDBWithModels(t, "config_history_",
		&model.ServerConfig{},
		&model.ServerConfigHistory{},
	)

	// Create partial unique indexes for production-like behavior
	err := db.Exec(`
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

	return db
}

// TestConfigRepo_TrimHistory_Basic tests normal history cleanup.
func TestConfigRepo_TrimHistory_Basic(t *testing.T) {
	t.Parallel()
	db := setupConfigHistoryTestDB(t)
	repo := NewServerConfigRepo(db)
	ctx := context.Background()

	key := "test.trim.basic"

	// Insert 150 history records
	for i := 0; i < 150; i++ {
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    nil,
			NewValue:  datatypes.JSON(fmt.Sprintf(`{"value": %d}`, i)),
			Version:   i + 1,
			ChangedBy: uuid.New(),
			ChangedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		}
		err := db.WithContext(ctx).Create(history).Error
		require.NoError(t, err)
	}

	// Verify all 150 records inserted
	var countBefore int64
	err := db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countBefore).Error
	require.NoError(t, err)
	assert.Equal(t, int64(150), countBefore, "should have 150 records before trim")

	// Call TrimHistory to keep only 100 records
	err = repo.TrimHistory(ctx, key, nil, 100)
	require.NoError(t, err)

	// Verify 100 records remain
	var countAfter int64
	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countAfter).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countAfter, "should have 100 records after trim")

	// Verify the oldest 50 were deleted (by checking the version number)
	var oldestRemaining model.ServerConfigHistory
	err = db.Where("key = ? AND user_id IS NULL", key).
		Order("changed_at ASC").
		First(&oldestRemaining).Error
	require.NoError(t, err)

	// The oldest remaining should be version 51 (the 51st record)
	// Since versions start at 1, the 51st record has version 51
	assert.Equal(t, 51, oldestRemaining.Version,
		"oldest remaining record should be version 51 (the 51st inserted)")
}

// TestConfigRepo_TrimHistory_ExactlyMaxCount tests when count equals maxCount.
func TestConfigRepo_TrimHistory_ExactlyMaxCount(t *testing.T) {
	t.Parallel()
	db := setupConfigHistoryTestDB(t)
	repo := NewServerConfigRepo(db)
	ctx := context.Background()

	key := "test.trim.exact"

	// Insert exactly 100 history records
	for i := 0; i < 100; i++ {
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    nil,
			NewValue:  datatypes.JSON(fmt.Sprintf(`{"value": %d}`, i)),
			Version:   i + 1,
			ChangedBy: uuid.New(),
			ChangedAt: time.Now(),
		}
		err := db.WithContext(ctx).Create(history).Error
		require.NoError(t, err)
	}

	// Verify all 100 records inserted
	var countBefore int64
	err := db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countBefore).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countBefore)

	// Call TrimHistory with maxCount = 100 (exactly equal)
	err = repo.TrimHistory(ctx, key, nil, 100)
	require.NoError(t, err)

	// Verify still 100 records (no deletion)
	var countAfter int64
	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countAfter).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countAfter, "should still have 100 records when count equals maxCount")
}

// TestConfigRepo_TrimHistory_BelowMaxCount tests when count is less than maxCount.
func TestConfigRepo_TrimHistory_BelowMaxCount(t *testing.T) {
	t.Parallel()
	db := setupConfigHistoryTestDB(t)
	repo := NewServerConfigRepo(db)
	ctx := context.Background()

	key := "test.trim.below"

	// Insert 50 history records
	for i := 0; i < 50; i++ {
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    nil,
			NewValue:  datatypes.JSON(fmt.Sprintf(`{"value": %d}`, i)),
			Version:   i + 1,
			ChangedBy: uuid.New(),
			ChangedAt: time.Now(),
		}
		err := db.WithContext(ctx).Create(history).Error
		require.NoError(t, err)
	}

	// Verify all 50 records inserted
	var countBefore int64
	err := db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countBefore).Error
	require.NoError(t, err)
	assert.Equal(t, int64(50), countBefore)

	// Call TrimHistory with maxCount = 100 (more than current count)
	err = repo.TrimHistory(ctx, key, nil, 100)
	require.NoError(t, err)

	// Verify still 50 records (no deletion)
	var countAfter int64
	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countAfter).Error
	require.NoError(t, err)
	assert.Equal(t, int64(50), countAfter, "should still have 50 records when count below maxCount")
}

// TestConfigRepo_TrimHistory_WithUserID tests cleanup with specific user_id.
func TestConfigRepo_TrimHistory_WithUserID(t *testing.T) {
	t.Parallel()
	db := setupConfigHistoryTestDB(t)
	repo := NewServerConfigRepo(db)
	ctx := context.Background()

	key := "test.trim.user"
	userID_A := uuid.New()
	userID_B := uuid.New()

	// Insert 100 records for user A
	for i := 0; i < 100; i++ {
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    &userID_A,
			NewValue:  datatypes.JSON(fmt.Sprintf(`{"user": "A", "value": %d}`, i)),
			Version:   i + 1,
			ChangedBy: uuid.New(),
			ChangedAt: time.Now(),
		}
		err := db.WithContext(ctx).Create(history).Error
		require.NoError(t, err)
	}

	// Insert 100 records for user B
	for i := 0; i < 100; i++ {
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    &userID_B,
			NewValue:  datatypes.JSON(fmt.Sprintf(`{"user": "B", "value": %d}`, i)),
			Version:   i + 1,
			ChangedBy: uuid.New(),
			ChangedAt: time.Now(),
		}
		err := db.WithContext(ctx).Create(history).Error
		require.NoError(t, err)
	}

	// Verify both users have 100 records
	var countA_Before, countB_Before int64
	err := db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id = ?", key, userID_A).
		Count(&countA_Before).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countA_Before)

	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id = ?", key, userID_B).
		Count(&countB_Before).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countB_Before)

	// Trim user A's history to 50 records
	err = repo.TrimHistory(ctx, key, &userID_A, 50)
	require.NoError(t, err)

	// Verify user A has 50 records remaining
	var countA_After int64
	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id = ?", key, userID_A).
		Count(&countA_After).Error
	require.NoError(t, err)
	assert.Equal(t, int64(50), countA_After, "user A should have 50 records after trim")

	// Verify user B still has 100 records (unaffected)
	var countB_After int64
	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id = ?", key, userID_B).
		Count(&countB_After).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countB_After, "user B should still have 100 records (unaffected)")
}

// TestConfigRepo_TrimHistory_Concurrent tests concurrent TrimHistory calls.
func TestConfigRepo_TrimHistory_Concurrent(t *testing.T) {
	t.Parallel()
	db := setupConfigHistoryTestDB(t)
	repo := NewServerConfigRepo(db)
	ctx := context.Background()

	key := "test.trim.concurrent"

	// Insert 200 history records
	for i := 0; i < 200; i++ {
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    nil,
			NewValue:  datatypes.JSON(fmt.Sprintf(`{"value": %d}`, i)),
			Version:   i + 1,
			ChangedBy: uuid.New(),
			ChangedAt: time.Now().Add(time.Duration(i) * time.Microsecond),
		}
		err := db.WithContext(ctx).Create(history).Error
		require.NoError(t, err)
	}

	// Verify 200 records inserted
	var countBefore int64
	err := db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countBefore).Error
	require.NoError(t, err)
	assert.Equal(t, int64(200), countBefore)

	// Launch 10 goroutines to concurrently call TrimHistory
	numGoroutines := 10
	errors := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			err := repo.TrimHistory(ctx, key, nil, 100)
			errors <- err
		}()
	}

	// Collect results
	for i := 0; i < numGoroutines; i++ {
		err := <-errors
		require.NoError(t, err, "concurrent TrimHistory should not error")
	}

	// Verify final count is 100 records
	var countAfter int64
	err = db.Model(&model.ServerConfigHistory{}).
		Where("key = ? AND user_id IS NULL", key).
		Count(&countAfter).Error
	require.NoError(t, err)
	assert.Equal(t, int64(100), countAfter, "should have 100 records after concurrent trims")

	// Verify no panic or deadlock occurred (test completion itself proves this)
}
