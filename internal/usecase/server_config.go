// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"encoding/json"
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

// Sentinel errors for server config operations.
var (
	ErrConfigKeyNotFound = errors.New("config key not found in registry")
	ErrVersionNotFound   = errors.New("target version not found in history")
	ErrNoOp              = errors.New("no operation needed")
	ErrUserNotFound      = errors.New("user not found")
	ErrConfigNotFound    = errors.New("config not found in database")
	ErrOptimisticLock    = errors.New("optimistic lock conflict")
	ErrValidation        = errors.New("config validation failed")
)

// ServerConfigUsecase handles dynamic configuration management.
type ServerConfigUsecase struct {
	configRepo     repo.ConfigRepo
	auditLogRepo   repo.AuditLogRepo
	oauth2UserRepo repo.OAuth2UserRepo
	db             *gorm.DB
}

// NewServerConfigUsecase creates a new ServerConfigUsecase.
func NewServerConfigUsecase(configRepo repo.ConfigRepo, auditLogRepo repo.AuditLogRepo, oauth2UserRepo repo.OAuth2UserRepo, db *gorm.DB) *ServerConfigUsecase {
	return &ServerConfigUsecase{
		configRepo:     configRepo,
		auditLogRepo:   auditLogRepo,
		oauth2UserRepo: oauth2UserRepo,
		db:             db,
	}
}

// SystemConfigItem represents a single system config entry in the list response.
type SystemConfigItem struct {
	Key         string  `json:"key"`
	Value       any     `json:"value"`
	ValueType   string  `json:"value_type"`
	Category    string  `json:"category"`
	Description string  `json:"description"`
	YamlDefault any     `json:"yaml_default"`
	Source      string  `json:"source"` // "yaml" | "system"
	Version     int     `json:"version"`
	UpdatedBy   *string `json:"updated_by,omitempty"`
	UpdatedAt   *string `json:"updated_at,omitempty"`
}

// UserConfigItem represents a single user config entry in the user config view.
type UserConfigItem struct {
	Key            string  `json:"key"`
	Category       string  `json:"category"`
	Description    string  `json:"description"`
	ValueType      string  `json:"value_type"`
	EffectiveValue any     `json:"effective_value"`
	Source         string  `json:"source"` // "yaml" | "system" | "user"
	YamlDefault    any     `json:"yaml_default"`
	SystemValue    any     `json:"system_value"`
	UserValue      any     `json:"user_value"`
	Version        int     `json:"version"` // user override version; 0 if no override
	UpdatedAt      *string `json:"updated_at,omitempty"`
}

// ConfigHistoryItem represents a single config history entry.
type ConfigHistoryItem struct {
	Version    int    `json:"version"`
	OldValue   any    `json:"old_value"`
	NewValue   any    `json:"new_value"`
	ChangedBy  string `json:"changed_by"`
	ChangedAt  string `json:"changed_at"`
	ChangeNote string `json:"change_note,omitempty"`
}

// UpdateConfigInput defines the input for updating a config value.
type UpdateConfigInput struct {
	Value      any    `json:"value"`
	Version    int    `json:"version"`
	ChangeNote string `json:"change_note"`
}

// RollbackConfigInput defines the input for rolling back to a historical version.
type RollbackConfigInput struct {
	TargetVersion int    `json:"target_version"`
	Version       int    `json:"version"`
	ChangeNote    string `json:"change_note"`
}

// ListSystemConfigs returns all registered config keys merged with DB values.
func (uc *ServerConfigUsecase) ListSystemConfigs(ctx context.Context, category string) ([]SystemConfigItem, int, error) {
	// Get all DB records for system configs.
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
	var items []SystemConfigItem
	for _, key := range config.AllRegistryKeys() {
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
			item.Value = unmarshalJSONB(dbCfg.Value)
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
	return items, len(items), nil
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
		item.Value = unmarshalJSONB(dbCfg.Value)
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

// UpdateSystemConfig updates a system-level config entry.
// operatorID and operatorIP are used for audit logging.
func (uc *ServerConfigUsecase) UpdateSystemConfig(ctx context.Context, key string, input UpdateConfigInput, operatorID uuid.UUID, operatorIP string) (*SystemConfigItem, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, ErrConfigKeyNotFound
	}

	// Validate and marshal value.
	jsonVal, err := marshalAndValidateValue(input.Value, entry)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	newVersion := input.Version + 1

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

	// Execute in transaction: get old value + upsert config + history + audit log.
	// getOldValue runs inside the transaction to avoid TOCTOU: the old value is read
	// atomically with the write, ensuring history consistency.
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// Capture old value within the same transaction for history.
		oldValueJSON := uc.getOldValue(txCtx, key, nil)

		if err := uc.configRepo.Upsert(txCtx, cfg, input.Version); err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("upsert config: %w", err)
		}

		// Write history.
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

		// Trim history to 100 entries.
		if err := uc.configRepo.TrimHistory(txCtx, key, nil, 100); err != nil {
			logger.Warn(ctx, "config.trim_history_failed", zap.Error(err))
		}

		// Audit log (config key stored in details, ResourceID left as zero UUID).
		auditDetails := map[string]any{
			"key":         key,
			"user_id":     nil,
			"old_value":   unmarshalJSONB(oldValueJSON),
			"new_value":   input.Value,
			"version":     newVersion,
			"change_note": input.ChangeNote,
		}
		auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.update", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create audit log: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return uc.GetSystemConfig(ctx, key)
}

// DeleteSystemConfig deletes a system config (reverts to yaml default).
func (uc *ServerConfigUsecase) DeleteSystemConfig(ctx context.Context, key string, version int, operatorID uuid.UUID, operatorIP string) (deletedValue any, deletedVersion int, err error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}

	now := time.Now()
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// CAS delete returns the deleted record in a single SELECT+DELETE,
		// eliminating the redundant pre-transaction Get (P2-1 fix).
		deletedCfg, delErr := uc.configRepo.Delete(txCtx, key, nil, version)
		if delErr != nil {
			if errors.Is(delErr, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("delete config: %w", delErr)
		}

		oldValueJSON := deletedCfg.Value
		deletedValue = unmarshalJSONB(oldValueJSON)
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
		auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.delete", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create audit log on delete: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	return deletedValue, deletedVersion, nil
}

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
	var items []UserConfigItem
	for _, key := range config.AllRegistryKeys() {
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
			item.SystemValue = unmarshalJSONB(sysCfg.Value)
		}
		if userCfg != nil {
			item.UserValue = unmarshalJSONB(userCfg.Value)
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
	if sysErr == nil {
		item.SystemValue = unmarshalJSONB(sysCfg.Value)
	}
	// User DB value.
	userCfg, userErr := uc.configRepo.Get(ctx, key, &userID)
	if userErr == nil {
		item.UserValue = unmarshalJSONB(userCfg.Value)
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
func (uc *ServerConfigUsecase) SetUserConfigOverride(ctx context.Context, userID uuid.UUID, key string, input UpdateConfigInput, operatorID uuid.UUID, operatorIP string) (*UserConfigItem, error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, ErrConfigKeyNotFound
	}

	// Verify user exists.
	if _, err := uc.oauth2UserRepo.FindByID(ctx, userID); err != nil {
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrOAuth2UserNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("check user existence: %w", err)
	}

	// Validate and marshal value.
	jsonVal, err := marshalAndValidateValue(input.Value, entry)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	newVersion := input.Version + 1

	cfg := &model.ServerConfig{
		Key:         key,
		UserID:      &userID,
		Value:       datatypes.JSON(jsonVal),
		ValueType:   entry.ValueType,
		Category:    entry.Category,
		Description: entry.Description,
		Version:     newVersion,
		UpdatedBy:   &operatorID,
		UpdatedAt:   now,
	}

	// Execute in transaction: get old value + upsert config + history + audit log.
	// getOldValue runs inside the transaction to avoid TOCTOU: the old value is read
	// atomically with the write, ensuring history consistency (P2-2 fix).
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// Capture old value within the same transaction for history.
		oldValueJSON := uc.getOldValue(txCtx, key, &userID)

		if err := uc.configRepo.Upsert(txCtx, cfg, input.Version); err != nil {
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
			Version:    newVersion,
			ChangedBy:  operatorID,
			ChangedAt:  now,
			ChangeNote: input.ChangeNote,
		}
		if err := tx.WithContext(ctx).Create(history).Error; err != nil {
			return fmt.Errorf("create user config history: %w", err)
		}

		if err := uc.configRepo.TrimHistory(txCtx, key, &userID, 100); err != nil {
			logger.Warn(ctx, "config.trim_history_failed", zap.Error(err))
		}

		auditDetails := map[string]any{
			"key":         key,
			"user_id":     userID.String(),
			"old_value":   unmarshalJSONB(oldValueJSON),
			"new_value":   input.Value,
			"version":     newVersion,
			"change_note": input.ChangeNote,
		}
		auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.update", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create audit log: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return uc.GetUserConfig(ctx, userID, key)
}

// DeleteUserConfigOverride deletes a user-level config override.
func (uc *ServerConfigUsecase) DeleteUserConfigOverride(ctx context.Context, userID uuid.UUID, key string, version int, operatorID uuid.UUID, operatorIP string) (deletedValue any, deletedVersion int, err error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}

	now := time.Now()
	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		// CAS delete returns the deleted record in a single SELECT+DELETE,
		// eliminating the redundant pre-transaction Get (P2-1 fix).
		deletedCfg, delErr := uc.configRepo.Delete(txCtx, key, &userID, version)
		if delErr != nil {
			if errors.Is(delErr, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("delete user config: %w", delErr)
		}

		oldValueJSON := deletedCfg.Value
		deletedValue = unmarshalJSONB(oldValueJSON)
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
		auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.delete", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create audit log on delete: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	return deletedValue, deletedVersion, nil
}

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
			OldValue:   unmarshalJSONB(h.OldValue),
			NewValue:   unmarshalJSONB(h.NewValue),
			ChangedBy:  h.ChangedBy.String(),
			ChangedAt:  h.ChangedAt.Format(time.RFC3339),
			ChangeNote: h.ChangeNote,
		})
	}
	return items, total, nil
}

// RollbackConfig rolls back a config to a historical version.
func (uc *ServerConfigUsecase) RollbackConfig(ctx context.Context, key string, userID *uuid.UUID, input RollbackConfigInput, operatorID uuid.UUID, operatorIP string) (rolledBackValue any, newVersion int, err error) {
	entry := config.GetRegistryEntry(key)
	if entry == nil {
		return nil, 0, ErrConfigKeyNotFound
	}

	// Get current config.
	currentCfg, getErr := uc.configRepo.Get(ctx, key, userID)
	if getErr != nil {
		if errors.Is(getErr, repo.ErrNotFound) {
			return nil, 0, ErrConfigNotFound
		}
		return nil, 0, fmt.Errorf("get config for rollback: %w", getErr)
	}

	// Check no-op.
	if input.TargetVersion == currentCfg.Version {
		return nil, 0, ErrNoOp
	}

	// Look up target version in history.
	targetHistory, histErr := uc.configRepo.GetHistoryByVersion(ctx, key, userID, input.TargetVersion)
	if histErr != nil {
		if errors.Is(histErr, repo.ErrNotFound) {
			return nil, 0, ErrVersionNotFound
		}
		return nil, 0, fmt.Errorf("get history for rollback: %w", histErr)
	}

	// Use the target version's new_value as the rollback value.
	targetValue := targetHistory.NewValue

	// Optimistic lock check.
	if input.Version != currentCfg.Version {
		return nil, 0, ErrOptimisticLock
	}

	now := time.Now()
	newVersion = input.Version + 1

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

	oldValueJSON := currentCfg.Value

	err = uc.db.Transaction(func(tx *gorm.DB) error {
		txCtx := repo.WithTx(ctx, tx)

		if upsertErr := uc.configRepo.Upsert(txCtx, cfg, input.Version); upsertErr != nil {
			if errors.Is(upsertErr, repo.ErrConflict) {
				return ErrOptimisticLock
			}
			return fmt.Errorf("upsert config for rollback: %w", upsertErr)
		}

		history := &model.ServerConfigHistory{
			Key:        key,
			UserID:     userID,
			OldValue:   oldValueJSON,
			NewValue:   targetValue,
			Version:    newVersion,
			ChangedBy:  operatorID,
			ChangedAt:  now,
			ChangeNote: input.ChangeNote,
		}
		if err := tx.WithContext(ctx).Create(history).Error; err != nil {
			return fmt.Errorf("create rollback history: %w", err)
		}

		if trimErr := uc.configRepo.TrimHistory(txCtx, key, userID, 100); trimErr != nil {
			logger.Warn(ctx, "config.trim_history_failed", zap.Error(trimErr))
		}

		auditDetails := map[string]any{
			"key":                      key,
			"user_id":                  userIDString(userID),
			"old_value":                unmarshalJSONB(oldValueJSON),
			"new_value":                unmarshalJSONB(targetValue),
			"rolled_back_from_version": currentCfg.Version,
			"target_version":           input.TargetVersion,
			"version":                  newVersion,
			"change_note":              input.ChangeNote,
		}
		auditLog := repo.NewAuditLog(operatorID, operatorIP, "config.rollback", "server_config", uuid.Nil, auditDetails)
		if err := uc.auditLogRepo.Create(txCtx, auditLog); err != nil {
			return fmt.Errorf("create rollback audit log: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	return unmarshalJSONB(targetValue), newVersion, nil
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
			return unmarshalJSONB(userCfg.Value), nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return nil, fmt.Errorf("get user config: %w", err)
		}
	}

	// Try system default.
	sysCfg, err := uc.configRepo.Get(ctx, key, nil)
	if err == nil {
		return unmarshalJSONB(sysCfg.Value), nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("get system config: %w", err)
	}

	// Fall back to yaml baseline.
	return entry.YamlDefault, nil
}

// ── Helpers ──────────────────────────────────────────────────────────

// getOldValue fetches the current JSON value for history tracking.
// Returns nil if the config does not exist (first write).
// Should be called within a transaction to ensure atomicity with the write operation (P2-2 fix).
func (uc *ServerConfigUsecase) getOldValue(ctx context.Context, key string, userID *uuid.UUID) datatypes.JSON {
	cfg, err := uc.configRepo.Get(ctx, key, userID)
	if err != nil {
		return nil
	}
	return cfg.Value
}

// marshalAndValidateValue marshals a value to JSON and validates it against the registry entry.
func marshalAndValidateValue(value any, entry *config.ConfigEntry) ([]byte, error) {
	// Type-specific conversion and validation.
	converted, err := convertValueByType(value, entry.ValueType)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	// Run custom validator if present.
	if entry.Validator != nil {
		if vErr := entry.Validator(converted); vErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrValidation, vErr)
		}
	}

	jsonVal, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal value: %v", ErrValidation, err)
	}
	return jsonVal, nil
}

// convertValueByType converts a raw JSON-decoded value to the expected Go type.
func convertValueByType(value any, valueType string) (any, error) {
	switch valueType {
	case config.ValueTypeString:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", value)
		}
		return s, nil
	case config.ValueTypeInt:
		return toInt(value)
	case config.ValueTypeFloat:
		return toFloat(value)
	case config.ValueTypeBool:
		b, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", value)
		}
		return b, nil
	case config.ValueTypeDuration:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected duration string, got %T", value)
		}
		if err := config.ValidateDuration(s); err != nil {
			return nil, err
		}
		return s, nil
	case config.ValueTypeJSON:
		return value, nil
	default:
		return nil, fmt.Errorf("unknown value_type: %s", valueType)
	}
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("expected int, got %v", v)
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("expected int, got %T", v)
	}
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, fmt.Errorf("expected float, got %v", v)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("expected float, got %T", v)
	}
}

// unmarshalJSONB safely unmarshals datatypes.JSON to a Go value.
func unmarshalJSONB(data datatypes.JSON) any {
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}

func userIDString(uid *uuid.UUID) any {
	if uid == nil {
		return nil
	}
	return uid.String()
}
