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

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
)

// GetHistory returns config change history.
func (uc *ServerConfigUsecase) GetHistory(ctx context.Context, key string, userID *uuid.UUID, page, pageSize int) ([]ConfigHistoryItem, int64, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}
	filter := model.ConfigHistoryFilter{Key: key, UserID: userID}
	histories, total, err := uc.configRepo.ListHistory(ctx, filter, page, pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list config history: %w", err)
	}
	items := make([]ConfigHistoryItem, 0, len(histories))
	for _, h := range histories {
		items = append(items, ConfigHistoryItem{
			Version:    h.Version,
			OldValue:   config.UnmarshalJSONB(h.OldValue),
			NewValue:   config.UnmarshalJSONB(h.NewValue),
			ChangedBy:  h.ChangedBy.String(),
			ChangedAt:  h.ChangedAt.Format(time.RFC3339),
			ChangeNote: h.ChangeNote,
		})
	}
	return items, total, nil
}

// RollbackConfig rolls back a config to a historical version.
// All reads (current config + target history) are performed inside the transaction
// to prevent TOCTOU races where a concurrent write invalidates the captured old value.
func (uc *ServerConfigUsecase) RollbackConfig(ctx context.Context, key string, userID *uuid.UUID, input RollbackConfigInput, operatorID uuid.UUID, operatorIP string) (rolledBackValue any, newVersion int, err error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}

	now := time.Now()
	var rolledBackJSON datatypes.JSON
	var shouldTrimHistory bool // Track if TrimHistory should be called after transaction

	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// Get current config inside transaction to avoid TOCTOU.
		currentCfg, getErr := uc.configRepo.Get(txCtx, key, userID)
		if getErr != nil {
			if errors.Is(getErr, repo.ErrNotFound) {
				return ErrConfigNotFound
			}
			return fmt.Errorf("get config for rollback: %w", getErr)
		}

		// Check no-op.
		if input.TargetVersion == currentCfg.Version {
			return ErrNoOp
		}

		// Optimistic lock check.
		if input.Version != currentCfg.Version {
			return ErrOptimisticLock
		}

		// Special case: target_version=0 means delete the config (revert to yaml default).
		if input.TargetVersion == 0 {
			var v0Err error
			newVersion, v0Err, shouldTrimHistory = uc.rollbackToV0(tx, txCtx, ctx, key, userID, input, operatorID, operatorIP, now)
			return v0Err
		}

		// Normal rollback to historical version.
		var histErr error
		rolledBackJSON, newVersion, histErr, shouldTrimHistory = uc.rollbackToHistoricalVersion(tx, txCtx, ctx, key, userID, entry, input, currentCfg, operatorID, operatorIP, now)
		return histErr
	})
	if err != nil {
		return nil, 0, err
	}

	// TrimHistory is called outside the transaction to reduce lock hold time and maintain
	// consistency with other config mutation methods (UpdateConfig, BatchUpdateConfigs).
	// This is safe because TrimHistory is idempotent and non-critical.
	if shouldTrimHistory {
		if trimErr := uc.configRepo.TrimHistory(ctx, key, userID, 100); trimErr != nil {
			logger.Warn(ctx, "config.trim_history_failed", zap.Error(trimErr))
		}
	}

	// Publish config rollback event for multi-instance cache synchronization.
	if uc.configPublisher != nil {
		if pubErr := uc.configPublisher.PublishConfigUpdate(ctx, key, userID, newVersion, "rollback"); pubErr != nil {
			logger.Warn(ctx, "config.publish_rollback_failed",
				zap.String("key", key),
				zap.Error(pubErr))
		}
	}

	return config.UnmarshalJSONB(rolledBackJSON), newVersion, nil
}

// rollbackToV0 handles the special case where target_version=0 (delete config).
// Returns the new version number, any error, and a flag indicating whether TrimHistory should be called.
func (uc *ServerConfigUsecase) rollbackToV0(tx *gorm.DB, txCtx context.Context, ctx context.Context, key string, userID *uuid.UUID, input RollbackConfigInput, operatorID uuid.UUID, operatorIP string, now time.Time) (int, error, bool) {
	// Delete the config record.
	deletedCfg, delErr := uc.configRepo.Delete(txCtx, key, userID, input.Version)
	if delErr != nil {
		if errors.Is(delErr, repo.ErrNotFound) {
			return 0, ErrConfigNotFound, false
		}
		if errors.Is(delErr, repo.ErrConflict) {
			return 0, ErrOptimisticLock, false
		}
		return 0, fmt.Errorf("delete config for rollback to v0: %w", delErr), false
	}

	newVersion := deletedCfg.Version + 1

	// Create history record for the deletion.
	history := &model.ServerConfigHistory{
		Key:        key,
		UserID:     userID,
		OldValue:   deletedCfg.Value,
		NewValue:   datatypes.JSON("null"),
		Version:    newVersion,
		ChangedBy:  operatorID,
		ChangedAt:  now,
		ChangeNote: input.ChangeNote,
	}
	if err := tx.WithContext(ctx).Create(history).Error; err != nil {
		return 0, fmt.Errorf("create rollback to v0 history: %w", err), false
	}

	auditDetails := map[string]any{
		"key":             key,
		"user_id":         userIDString(userID),
		"deleted_value":   config.UnmarshalJSONB(deletedCfg.Value),
		"deleted_version": deletedCfg.Version,
		"target_version":  0,
		"change_note":     input.ChangeNote,
	}
	auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.rollback", "server_config", uuid.Nil, auditDetails)
	if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
		return 0, fmt.Errorf("create rollback to v0 audit log: %w", err), false
	}

	// Return true to indicate TrimHistory should be called after transaction commits.
	// This is consistent with other config mutation methods and reduces transaction lock time.
	return newVersion, nil, true
}

// rollbackToHistoricalVersion handles rollback to a specific historical version.
// Returns the rolled back JSON value, the new version number, any error, and a flag indicating whether TrimHistory should be called.
func (uc *ServerConfigUsecase) rollbackToHistoricalVersion(tx *gorm.DB, txCtx context.Context, ctx context.Context, key string, userID *uuid.UUID, entry *config.ConfigEntry, input RollbackConfigInput, currentCfg *model.ServerConfig, operatorID uuid.UUID, operatorIP string, now time.Time) (datatypes.JSON, int, error, bool) {
	// Look up target version in history inside transaction.
	targetHistory, histErr := uc.configRepo.GetHistoryByVersion(txCtx, key, userID, input.TargetVersion)
	if histErr != nil {
		if errors.Is(histErr, repo.ErrNotFound) {
			return nil, 0, ErrVersionNotFound, false
		}
		return nil, 0, fmt.Errorf("get history for rollback: %w", histErr), false
	}

	targetValue := targetHistory.NewValue
	newVersion := input.Version + 1

	cfg := &model.ServerConfig{
		Key:         key,
		UserID:      userID,
		Value:       targetValue,
		ValueType:   entry.ValueType,
		Category:    entry.Category,
		Description: entry.Description,
		Version:     newVersion,
		UpdatedBy:   &operatorID,
		UpdatedAt:   now,
	}

	if upsertErr := uc.configRepo.Upsert(txCtx, cfg, input.Version); upsertErr != nil {
		if errors.Is(upsertErr, repo.ErrConflict) {
			return nil, 0, ErrOptimisticLock, false
		}
		return nil, 0, fmt.Errorf("upsert config for rollback: %w", upsertErr), false
	}

	history := &model.ServerConfigHistory{
		Key:        key,
		UserID:     userID,
		OldValue:   currentCfg.Value,
		NewValue:   targetValue,
		Version:    newVersion,
		ChangedBy:  operatorID,
		ChangedAt:  now,
		ChangeNote: input.ChangeNote,
	}
	if err := tx.WithContext(ctx).Create(history).Error; err != nil {
		return nil, 0, fmt.Errorf("create rollback history: %w", err), false
	}

	auditDetails := map[string]any{
		"key":                      key,
		"user_id":                  userIDString(userID),
		"old_value":                config.UnmarshalJSONB(currentCfg.Value),
		"new_value":                config.UnmarshalJSONB(targetValue),
		"rolled_back_from_version": currentCfg.Version,
		"target_version":           input.TargetVersion,
		"version":                  newVersion,
		"change_note":              input.ChangeNote,
	}
	auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.rollback", "server_config", uuid.Nil, auditDetails)
	if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
		return nil, 0, fmt.Errorf("create rollback audit log: %w", err), false
	}

	// Return true to indicate TrimHistory should be called after transaction commits.
	// This is consistent with other config mutation methods and reduces transaction lock time.
	return targetValue, newVersion, nil, true
}

// GetEffectiveValue returns the effective config value for a key and user.
// Priority: user override > system default > yaml baseline.
// If userID is nil, only system default > yaml is resolved.
func (uc *ServerConfigUsecase) GetEffectiveValue(ctx context.Context, key string, userID *uuid.UUID) (any, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, ErrConfigKeyNotFound
	}

	// Try user override first.
	if userID != nil {
		userCfg, err := uc.configRepo.Get(ctx, key, userID)
		if err == nil {
			return config.UnmarshalJSONB(userCfg.Value), nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return nil, fmt.Errorf("get user config: %w", err)
		}
	}

	// Try system default.
	sysCfg, err := uc.configRepo.Get(ctx, key, nil)
	if err == nil {
		return config.UnmarshalJSONB(sysCfg.Value), nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("get system config: %w", err)
	}

	// Fall back to yaml baseline.
	return entry.YamlDefault, nil
}
