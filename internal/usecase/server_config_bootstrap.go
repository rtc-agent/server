package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/datatypes"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// BootstrapDynamicConfigs initializes all dynamic configs from the registry into the DB.
// This ensures "out-of-box" experience: new deployments have all configs ready for editing.
//
// For each config key that doesn't exist in DB:
//  1. INSERT config record (version=1, value=yaml_default)
//  2. INSERT history record v1 (version=1, old_value=null, new_value=yaml_default)
//
// This function is idempotent: existing configs are not modified.
// operatorID is used for audit trail (use uuid.Nil for system bootstrap).
func (uc *ServerConfigUsecase) BootstrapDynamicConfigs(ctx context.Context, operatorID uuid.UUID) error {
	now := time.Now()

	for _, key := range config.AllRegistryKeys() {
		entry := config.GetRegistryEntry(key)
		if entry == nil {
			continue
		}

		// Check if system config already exists.
		_, err := uc.configRepo.Get(ctx, key, nil)
		if err == nil {
			// Already exists, skip.
			continue
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return fmt.Errorf("check config %q existence: %w", key, err)
		}

		// Config doesn't exist, bootstrap it.
		// Marshal yaml default to JSON.
		jsonVal, err := marshalAndValidateValue(entry.YamlDefault, entry)
		if err != nil {
			// If yaml default is invalid, log and skip (should not happen in practice).
			logger.Warn(ctx, "bootstrap.invalid_yaml_default",
				zap.String("key", key),
				zap.Any("yaml_default", entry.YamlDefault),
				zap.Error(err))
			continue
		}

		// Create config record (version=1).
		cfg := &model.ServerConfig{
			Key:         key,
			UserID:      nil,
			Value:       datatypes.JSON(jsonVal),
			ValueType:   entry.ValueType,
			Category:    entry.Category,
			Description: entry.Description,
			Version:     1,
			UpdatedBy:   &operatorID,
			UpdatedAt:   now,
		}
		if err := uc.configRepo.Upsert(ctx, cfg, 0); err != nil {
			// In multi-instance deployments, concurrent bootstrap attempts may cause
			// unique constraint violations. If the config was already created by another
			// instance, skip silently (idempotent behavior).
			if errors.Is(err, repo.ErrConflict) {
				logger.Debug(ctx, "bootstrap.config_already_exists",
					zap.String("key", key),
					zap.String("reason", "concurrent_bootstrap"))
				continue
			}
			return fmt.Errorf("bootstrap config %q: %w", key, err)
		}

		// Create v1 history record (old_value=null, new_value=yaml_default).
		// This represents the "initial bootstrap" from no DB value to yaml default.
		history := &model.ServerConfigHistory{
			Key:        key,
			UserID:     nil,
			OldValue:   nil, // null represents "no DB value"
			NewValue:   datatypes.JSON(jsonVal),
			Version:    1, // Match config record version
			ChangedBy:  operatorID,
			ChangedAt:  now,
			ChangeNote: "Initial bootstrap from yaml default",
		}
		if err := uc.db.WithContext(ctx).Create(history).Error; err != nil {
			// History creation may also fail due to concurrent bootstrap or duplicate key.
			// Log as warning but don't fail the entire bootstrap process.
			if repo.IsDuplicateKeyError(err) {
				logger.Warn(ctx, "bootstrap.history_already_exists",
					zap.String("key", key),
					zap.Error(err))
				continue
			}
			return fmt.Errorf("create bootstrap history for %q: %w", key, err)
		}

		logger.Info(ctx, "bootstrap.config_initialized",
			zap.String("key", key),
			zap.Any("value", entry.YamlDefault))
	}

	return nil
}
