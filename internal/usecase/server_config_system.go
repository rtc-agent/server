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

// ListSystemConfigs returns registered config keys merged with DB values, with pagination.
// The registry keys are the source of truth for which configs exist; DB records provide
// overrides. Pagination is applied in-memory after merging, since the full key set is
// small (~50 keys) and must be iterated to produce the unified view.
func (uc *ServerConfigUsecase) ListSystemConfigs(ctx context.Context, category string, page, pageSize int) ([]SystemConfigItem, int, error) {
	// Get all DB records for system configs.
	// DB-level category filter reduces data fetched; the loop filter below is still
	// necessary because we iterate all registry keys (including those without DB records).
	filter := model.ConfigFilter{Category: category}
	dbConfigs, err := uc.configRepo.List(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("list system configs: %w", err)
	}
	dbMap := make(map[string]*model.ServerConfig, len(dbConfigs))
	for _, c := range dbConfigs {
		dbMap[c.Key] = c
	}

	// Iterate registry keys filtered by category.
	// The loop filter is necessary (not redundant with the DB filter above) because
	// we iterate ALL registry keys, including those without DB records. The DB filter
	// only reduces the data fetched; the loop filter controls which keys appear in output.
	keys := config.AllRegistryKeys()
	items := make([]SystemConfigItem, 0, len(keys)) // Pre-allocate to avoid reallocations
	for _, key := range keys {
		entry := config.GetRegistryEntry(key)
		if entry == nil {
			continue
		}
		if category != "" && entry.Category != category {
			continue
		}
		item := SystemConfigItem{
			Key:         key,
			ValueType:   entry.ValueType,
			Category:    entry.Category,
			Description: entry.Description,
			YamlDefault: entry.YamlDefault,
		}
		if dbCfg, ok := dbMap[key]; ok {
			item.Source = "system"
			item.Value = config.UnmarshalJSONB(dbCfg.Value)
			item.Version = dbCfg.Version
			if dbCfg.UpdatedBy != nil {
				s := dbCfg.UpdatedBy.String()
				item.UpdatedBy = &s
			}
			ts := dbCfg.UpdatedAt.Format(time.RFC3339)
			item.UpdatedAt = &ts
		} else {
			item.Source = "yaml"
			item.Value = nil
			item.Version = 0
		}
		items = append(items, item)
	}

	// Paginate the merged result.
	total := len(items)
	items = paginateItems(items, page, pageSize)
	return items, total, nil
}

// GetSystemConfig returns a single system config by key.
func (uc *ServerConfigUsecase) GetSystemConfig(ctx context.Context, key string) (*SystemConfigItem, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, ErrConfigKeyNotFound
	}
	item := &SystemConfigItem{
		Key:         key,
		ValueType:   entry.ValueType,
		Category:    entry.Category,
		Description: entry.Description,
		YamlDefault: entry.YamlDefault,
		Source:      "yaml",
		Value:       nil,
		Version:     0,
	}
	dbCfg, err := uc.configRepo.Get(ctx, key, nil)
	if err == nil {
		item.Source = "system"
		item.Value = config.UnmarshalJSONB(dbCfg.Value)
		item.Version = dbCfg.Version
		if dbCfg.UpdatedBy != nil {
			s := dbCfg.UpdatedBy.String()
			item.UpdatedBy = &s
		}
		ts := dbCfg.UpdatedAt.Format(time.RFC3339)
		item.UpdatedAt = &ts
	} else if !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("get system config: %w", err)
	}
	return item, nil
}

// UpdateSystemConfig updates a system-level dynamic config entry.
// operatorID and operatorIP are used for audit logging.
// input.Version < 0 means force overwrite (still uses optimistic lock with current version).
//
// Retry: On optimistic lock conflict, automatically retries up to 3 times.
// This handles the common case of concurrent config updates from different admin users.
func (uc *ServerConfigUsecase) UpdateSystemConfig(ctx context.Context, key string, input UpdateConfigInput, operatorID uuid.UUID, operatorIP string) (*SystemConfigItem, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, ErrConfigKeyNotFound
	}

	// Validate and marshal value (outside retry loop — value doesn't change between retries).
	jsonVal, err := marshalAndValidateValue(input.Value, entry)
	if err != nil {
		return nil, err
	}

	const maxConfigRetries = 3
	now := time.Now()

	for attempt := 0; attempt < maxConfigRetries; attempt++ {
		_, newVersion, err := uc.executeConfigUpdateTx(ctx, key, jsonVal, input, entry, operatorID, operatorIP, now)
		if err == nil {
			uc.postUpdateOperations(ctx, key, newVersion)
			return uc.GetSystemConfig(ctx, key)
		}

		if !errors.Is(err, ErrOptimisticLock) {
			return nil, err
		}

		// P0-4 fix: Only retry when input.Version < 0 (force overwrite).
		// When input.Version >= 0, the client specified a specific version that has changed,
		// so retrying won't help - the client must fetch the new version and retry explicitly.
		if input.Version >= 0 {
			return nil, fmt.Errorf("optimistic lock conflict: specified version %d no longer current for key %q", input.Version, key)
		}

		if attempt < maxConfigRetries-1 {
			// P0-4 fix: Add exponential backoff to reduce contention.
			backoff := time.Duration(1<<uint(attempt)) * 100 * time.Millisecond // 100ms, 200ms, 400ms
			logger.Warn(ctx, "optimistic_lock_conflict_retrying",
				zap.String("key", key),
				zap.Int("attempt", attempt+1),
				zap.Int("max_retries", maxConfigRetries),
				zap.Duration("backoff", backoff))
			time.Sleep(backoff)
			continue
		}
	}

	return nil, fmt.Errorf("optimistic lock conflict: max retries (%d) exhausted for key %q", maxConfigRetries, key)
}

// executeConfigUpdateTx performs the core update operation within a transaction.
// Returns the old value, new version, and any error encountered.
func (uc *ServerConfigUsecase) executeConfigUpdateTx(
	ctx context.Context,
	key string,
	jsonVal []byte,
	input UpdateConfigInput,
	entry *config.ConfigEntry,
	operatorID uuid.UUID,
	operatorIP string,
	now time.Time,
) (oldValueJSON datatypes.JSON, newVersion int, err error) {
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := dbutil.WithTx(ctx, tx)

		// Capture old value within the same transaction for history.
		oldValueJSON = uc.getOldValue(txCtx, key, nil, entry)

		expectedVersion := uc.calculateVersionAndExpected(input, oldValueJSON, key, txCtx, &newVersion)

		cfg := &model.ServerConfig{
			Key:         key,
			UserID:      nil,
			Value:       datatypes.JSON(jsonVal),
			ValueType:   entry.ValueType,
			Category:    entry.Category,
			Description: entry.Description,
			Version:     newVersion,
			UpdatedBy:   &operatorID,
			UpdatedAt:   now,
		}

		if err := uc.configRepo.Upsert(txCtx, cfg, expectedVersion); err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("upsert config: %w", err)
		}

		// Write history (must be in tx for data consistency with config).
		history := &model.ServerConfigHistory{
			Key:        key,
			UserID:     nil,
			OldValue:   oldValueJSON,
			NewValue:   datatypes.JSON(jsonVal),
			Version:    newVersion,
			ChangedBy:  operatorID,
			ChangedAt:  now,
			ChangeNote: input.ChangeNote,
		}
		if err := tx.WithContext(ctx).Create(history).Error; err != nil {
			return fmt.Errorf("create config history: %w", err)
		}

		// Write audit log within the same transaction for atomicity (P0-1 fix).
		auditDetails := map[string]any{
			"key":         key,
			"user_id":     nil,
			"old_value":   config.UnmarshalJSONB(oldValueJSON),
			"new_value":   input.Value,
			"version":     newVersion,
			"change_note": input.ChangeNote,
		}
		auditLog := repo.NewAuditLog(ctx, operatorID, operatorIP, "config.update", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create audit log: %w", err)
		}

		return nil
	})
	return oldValueJSON, newVersion, err
}

// calculateVersionAndExpected determines the new version and expected version for optimistic locking.
func (uc *ServerConfigUsecase) calculateVersionAndExpected(
	input UpdateConfigInput,
	oldValueJSON datatypes.JSON,
	key string,
	txCtx context.Context,
	newVersion *int,
) int {
	if input.Version < 0 {
		// Force overwrite: use current version for optimistic lock instead of skipping it.
		currentVersion, newVer, err := uc.calculateForceOverwriteVersion(txCtx, key, nil, oldValueJSON)
		if err != nil {
			return 0
		}
		*newVersion = newVer
		return currentVersion // Use currentVersion, not -1
	}

	*newVersion = input.Version + 1
	return input.Version
}

// postUpdateOperations handles all operations after a successful config update transaction.
// P0-1 fix: Audit log is now created within the transaction for atomicity.
// P0-2 fix: Publish operations now include retry with exponential backoff.
func (uc *ServerConfigUsecase) postUpdateOperations(
	ctx context.Context,
	key string,
	newVersion int,
) {
	// Trim history to 100 entries (non-critical, failures logged).
	if trimErr := uc.configRepo.TrimHistory(ctx, key, nil, 100); trimErr != nil {
		logger.Warn(ctx, "config.trim_history_failed", zap.Error(trimErr))
	}

	// Publish config update event for multi-instance cache synchronization.
	// P0-2 fix: Add retry with exponential backoff for publish operations.
	if uc.configPublisher != nil {
		const maxPublishRetries = 3
		var pubErr error
		for attempt := 0; attempt < maxPublishRetries; attempt++ {
			pubErr = uc.configPublisher.PublishConfigUpdate(ctx, key, nil, newVersion, "update")
			if pubErr == nil {
				break
			}
			if attempt < maxPublishRetries-1 {
				backoff := time.Duration(1<<uint(attempt)) * 50 * time.Millisecond // 50ms, 100ms
				logger.Warn(ctx, "config.publish_update_failed_retrying",
					zap.String("key", key),
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
				zap.Int("version", newVersion),
				zap.Error(pubErr))
			globalPublisherMetrics.publishErrors.Inc()
		}
	}
}

// DeleteSystemConfig deletes a system config (reverts to yaml default).
func (uc *ServerConfigUsecase) DeleteSystemConfig(ctx context.Context, key string, version int, operatorID uuid.UUID, operatorIP string) (deletedValue any, deletedVersion int, err error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}

	now := time.Now()
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := dbutil.WithTx(ctx, tx)

		// CAS delete returns the deleted record in a single SELECT+DELETE,
		// eliminating the redundant pre-transaction Get (P2-1 fix).
		deletedCfg, delErr := uc.configRepo.Delete(txCtx, key, nil, version)
		if delErr != nil {
			if errors.Is(delErr, repo.ErrNotFound) {
				return ErrConfigNotFound
			}
			if errors.Is(delErr, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("delete config: %w", delErr)
		}

		oldValueJSON := deletedCfg.Value
		deletedValue = config.UnmarshalJSONB(oldValueJSON)
		deletedVersion = deletedCfg.Version

		// Write history.
		history := &model.ServerConfigHistory{
			Key:       key,
			UserID:    nil,
			OldValue:  oldValueJSON,
			NewValue:  datatypes.JSON("null"),
			Version:   deletedCfg.Version + 1,
			ChangedBy: operatorID,
			ChangedAt: now,
		}
		if err := tx.WithContext(ctx).Create(history).Error; err != nil {
			return fmt.Errorf("create config history on delete: %w", err)
		}

		// Audit log.
		auditDetails := map[string]any{
			"key":             key,
			"user_id":         nil,
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
	// P0-2 fix: Add retry with exponential backoff for publish operations.
	if uc.configPublisher != nil {
		const maxPublishRetries = 3
		var pubErr error
		for attempt := 0; attempt < maxPublishRetries; attempt++ {
			pubErr = uc.configPublisher.PublishConfigUpdate(ctx, key, nil, deletedVersion+1, "delete")
			if pubErr == nil {
				break
			}
			if attempt < maxPublishRetries-1 {
				backoff := time.Duration(1<<uint(attempt)) * 50 * time.Millisecond // 50ms, 100ms
				logger.Warn(ctx, "config.publish_delete_failed_retrying",
					zap.String("key", key),
					zap.Int("attempt", attempt+1),
					zap.Int("max_retries", maxPublishRetries),
					zap.Duration("backoff", backoff),
					zap.Error(pubErr))
				time.Sleep(backoff)
			}
		}
		if pubErr != nil {
			logger.Error(ctx, "config.publish_delete_failed_after_retries",
				zap.String("key", key),
				zap.Int("version", deletedVersion+1),
				zap.Error(pubErr))
			globalPublisherMetrics.publishErrors.Inc()
		}
	}

	return deletedValue, deletedVersion, nil
}
