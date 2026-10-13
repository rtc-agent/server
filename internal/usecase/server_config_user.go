package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/dbutil"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// GetUserConfigsView returns the complete config view for a user (yaml + system + user merged).
func (uc *ServerConfigUsecase) GetUserConfigsView(ctx context.Context, userID uuid.UUID, category string) ([]UserConfigItem, error) {
	// Get system DB configs.
	sysFilter := model.ConfigFilter{Category: category}
	sysConfigs, err := uc.configRepo.List(ctx, sysFilter)
	if err != nil {
		return nil, fmt.Errorf("list system configs: %w", err)
	}
	sysMap := make(map[string]*model.ServerConfig, len(sysConfigs))
	for _, c := range sysConfigs {
		sysMap[c.Key] = c
	}

	// Get user DB configs.
	userFilter := model.ConfigFilter{Category: category, UserID: &userID}
	userConfigs, err := uc.configRepo.List(ctx, userFilter)
	if err != nil {
		return nil, fmt.Errorf("list user configs: %w", err)
	}
	userMap := make(map[string]*model.ServerConfig, len(userConfigs))
	for _, c := range userConfigs {
		userMap[c.Key] = c
	}

	// Merge registry + system + user.
	// The loop filter is necessary (not redundant with the DB filters above) because
	// we iterate ALL registry keys, including those without DB records. The DB filters
	// only reduce the data fetched; the loop filter controls which keys appear in output.
	keys := config.AllRegistryKeys()
	items := make([]UserConfigItem, 0, len(keys)) // Pre-allocate to avoid reallocations
	for _, key := range keys {
		entry := config.GetRegistryEntry(key)
		if entry == nil {
			continue
		}
		if category != "" && entry.Category != category {
			continue
		}
		item := UserConfigItem{
			Key:         key,
			Category:    entry.Category,
			Description: entry.Description,
			ValueType:   entry.ValueType,
			YamlDefault: entry.YamlDefault,
		}
		sysCfg := sysMap[key]
		userCfg := userMap[key]

		if sysCfg != nil {
			item.SystemValue = config.UnmarshalJSONB(sysCfg.Value)
		}
		if userCfg != nil {
			item.UserValue = config.UnmarshalJSONB(userCfg.Value)
			item.Version = userCfg.Version
			ts := userCfg.UpdatedAt.Format(time.RFC3339)
			item.UpdatedAt = &ts
		}

		// Determine effective value and source.
		if userCfg != nil {
			item.EffectiveValue = item.UserValue
			item.Source = "user"
		} else if sysCfg != nil {
			item.EffectiveValue = item.SystemValue
			item.Source = "system"
		} else {
			item.EffectiveValue = entry.YamlDefault
			item.Source = "yaml"
		}

		items = append(items, item)
	}
	return items, nil
}

// GetUserConfig returns a single user config view item.
func (uc *ServerConfigUsecase) GetUserConfig(ctx context.Context, userID uuid.UUID, key string) (*UserConfigItem, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, ErrConfigKeyNotFound
	}
	item := &UserConfigItem{
		Key:            key,
		Category:       entry.Category,
		Description:    entry.Description,
		ValueType:      entry.ValueType,
		YamlDefault:    entry.YamlDefault,
		Source:         "yaml",
		EffectiveValue: entry.YamlDefault,
		Version:        0,
	}

	// System DB value.
	sysCfg, sysErr := uc.configRepo.Get(ctx, key, nil)
	if sysErr != nil && !errors.Is(sysErr, repo.ErrNotFound) {
		return nil, fmt.Errorf("get system config for user view: %w", sysErr)
	}
	if sysErr == nil {
		item.SystemValue = config.UnmarshalJSONB(sysCfg.Value)
	}
	// User DB value.
	userCfg, userErr := uc.configRepo.Get(ctx, key, &userID)
	if userErr != nil && !errors.Is(userErr, repo.ErrNotFound) {
		return nil, fmt.Errorf("get user config for user view: %w", userErr)
	}
	if userErr == nil {
		item.UserValue = config.UnmarshalJSONB(userCfg.Value)
		item.Version = userCfg.Version
		ts := userCfg.UpdatedAt.Format(time.RFC3339)
		item.UpdatedAt = &ts
	}

	// Effective value.
	if userCfg != nil && userErr == nil {
		item.EffectiveValue = item.UserValue
		item.Source = "user"
	} else if sysCfg != nil && sysErr == nil {
		item.EffectiveValue = item.SystemValue
		item.Source = "system"
	}

	return item, nil
}

// SetUserConfigOverride sets a user-level config override.
// input.Version < 0 means force overwrite (still uses optimistic lock with current version).
func (uc *ServerConfigUsecase) SetUserConfigOverride(ctx context.Context, userID uuid.UUID, key string, input UpdateConfigInput, operatorID uuid.UUID, operatorIP string) (*UserConfigItem, error) {
	// Validate input and prepare for transaction.
	entry, jsonVal, err := uc.validateAndPrepareConfigUpdate(ctx, userID, key, input)
	if err != nil {
		return nil, err
	}

	now := time.Now()

	// Variables to capture from transaction for post-tx operations.
	var newVersion int

	// Execute core operations in transaction: get old value + upsert config + history.
	if err := uc.executeConfigUpdateTransaction(ctx, userID, key, input, operatorID, operatorIP, entry, jsonVal, now, &newVersion); err != nil {
		return nil, err
	}

	// Post-transaction operations (non-core, failures logged but don't fail the operation).
	if err := uc.configRepo.TrimHistory(ctx, key, &userID, 100); err != nil {
		logger.Warn(ctx, "config.trim_history_failed", zap.Error(err))
	}

	// Publish config update event for multi-instance cache synchronization.
	uc.publishConfigUpdateWithRetry(ctx, key, &userID, newVersion, "update")

	return uc.GetUserConfig(ctx, userID, key)
}

// validateAndPrepareConfigUpdate validates the config update request and marshals the value.
func (uc *ServerConfigUsecase) validateAndPrepareConfigUpdate(ctx context.Context, userID uuid.UUID, key string, input UpdateConfigInput) (*config.ConfigEntry, []byte, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, nil, ErrConfigKeyNotFound
	}

	// Verify user exists.
	if _, err := uc.oauth2UserRepo.FindByID(ctx, userID); err != nil {
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrOAuth2UserNotFound) {
			return nil, nil, ErrUserNotFound
		}
		return nil, nil, fmt.Errorf("check user existence: %w", err)
	}

	// Validate and marshal value.
	jsonVal, err := marshalAndValidateValue(input.Value, entry)
	if err != nil {
		return nil, nil, err
	}

	return entry, jsonVal, nil
}

// executeConfigUpdateTransaction executes the config update within a transaction.
func (uc *ServerConfigUsecase) executeConfigUpdateTransaction(ctx context.Context, userID uuid.UUID, key string, input UpdateConfigInput, operatorID uuid.UUID, operatorIP string, entry *config.ConfigEntry, jsonVal []byte, now time.Time, newVersion *int) error {
	return uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := dbutil.WithTx(ctx, tx)

		// Capture old value within the same transaction for history.
		oldValueJSON := uc.getOldValue(txCtx, key, &userID, entry)

		var expectedVersion int
		if input.Version < 0 {
			// Force overwrite: use current version for optimistic lock instead of skipping it.
			currentVersion, newVer, err := uc.calculateForceOverwriteVersion(ctx, key, &userID, oldValueJSON)
			if err != nil {
				return err
			}
			*newVersion = newVer
			expectedVersion = currentVersion
		} else {
			*newVersion = input.Version + 1
			expectedVersion = input.Version
		}

		cfg := &model.ServerConfig{
			Key:         key,
			UserID:      &userID,
			Value:       datatypes.JSON(jsonVal),
			ValueType:   entry.ValueType,
			Category:    entry.Category,
			Description: entry.Description,
			Version:     *newVersion,
			UpdatedBy:   &operatorID,
			UpdatedAt:   now,
		}

		if err := uc.configRepo.Upsert(txCtx, cfg, expectedVersion); err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("upsert user config: %w", err)
		}

		history := &model.ServerConfigHistory{
			Key:        key,
			UserID:     &userID,
			OldValue:   oldValueJSON,
			NewValue:   datatypes.JSON(jsonVal),
			Version:    *newVersion,
			ChangedBy:  operatorID,
			ChangedAt:  now,
			ChangeNote: input.ChangeNote,
		}
		if err := tx.WithContext(ctx).Create(history).Error; err != nil {
			return fmt.Errorf("create user config history: %w", err)
		}

		// Create audit log within transaction for atomicity.
		// IMPORTANT: Both old_value and new_value are normalized for consistent formatting.
		auditDetails := map[string]any{
			"key":         key,
			"user_id":     userID.String(),
			"old_value":   config.UnmarshalJSONB(oldValueJSON),
			"new_value":   normalizeJSONValue(jsonVal),
			"version":     *newVersion,
			"change_note": input.ChangeNote,
		}
		auditLog := repo.NewAuditLog(ctx, operatorID, operatorIP, "config.update", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create user config audit log: %w", err)
		}

		return nil
	})
}

// publishConfigUpdateWithRetry publishes config update event with exponential backoff retry.
// P0-2 fix: Add retry for publish operations to handle transient failures.
func (uc *ServerConfigUsecase) publishConfigUpdateWithRetry(ctx context.Context, key string, userID *uuid.UUID, version int, operation string) {
	if uc.configPublisher == nil {
		return
	}

	const maxPublishRetries = 3
	var pubErr error
	for attempt := 0; attempt < maxPublishRetries; attempt++ {
		pubErr = uc.configPublisher.PublishConfigUpdate(ctx, key, userID, version, operation)
		if pubErr == nil {
			return
		}
		if attempt < maxPublishRetries-1 {
			backoff := time.Duration(1<<uint(attempt)) * 50 * time.Millisecond
			logger.Warn(ctx, "config.publish_update_failed_retrying",
				zap.String("key", key),
				zap.String("user_id", userID.String()),
				zap.Int("attempt", attempt+1),
				zap.Int("max_retries", maxPublishRetries),
				zap.Duration("backoff", backoff),
				zap.Error(pubErr))
			time.Sleep(backoff)
		}
	}

	if pubErr != nil {
		logger.Error(ctx, "config.publish_update_failed_after_retries",
			zap.String("key", key),
			zap.String("user_id", userID.String()),
			zap.Int("version", version),
			zap.Error(pubErr))
		globalPublisherMetrics.publishErrors.Inc()
	}
}

// DeleteUserConfigOverride deletes a user-level config override.
func (uc *ServerConfigUsecase) DeleteUserConfigOverride(ctx context.Context, userID uuid.UUID, key string, version int, operatorID uuid.UUID, operatorIP string) (deletedValue any, deletedVersion int, err error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}

	now := time.Now()
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := dbutil.WithTx(ctx, tx)

		// CAS delete returns the deleted record in a single SELECT+DELETE,
		// eliminating the redundant pre-transaction Get (P2-1 fix).
		deletedCfg, delErr := uc.configRepo.Delete(txCtx, key, &userID, version)
		if delErr != nil {
			if errors.Is(delErr, repo.ErrNotFound) {
				return ErrConfigNotFound
			}
			if errors.Is(delErr, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("delete user config: %w", delErr)
		}

		oldValueJSON := deletedCfg.Value
		deletedValue = config.UnmarshalJSONB(oldValueJSON)
		deletedVersion = deletedCfg.Version

		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    &userID,
			OldValue:  oldValueJSON,
			NewValue:  datatypes.JSON("null"),
			Version:   deletedCfg.Version + 1,
			ChangedBy: operatorID,
			ChangedAt: now,
		}
		if err := tx.WithContext(ctx).Create(history).Error; err != nil {
			return fmt.Errorf("create user config history on delete: %w", err)
		}

		auditDetails := map[string]any{
			"key":             key,
			"user_id":         userID.String(),
			"deleted_value":   deletedValue,
			"deleted_version": deletedVersion,
		}
		auditLog := repo.NewAuditLog(ctx, operatorID, operatorIP, "config.delete", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create audit log on delete: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Publish config delete event for multi-instance cache synchronization.
	uc.publishConfigUpdateWithRetry(ctx, key, &userID, deletedVersion+1, "delete")

	return deletedValue, deletedVersion, nil
}
