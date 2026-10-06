// Package repo provides data access layer (Repository) implementations.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
)

// ConfigRepo provides dynamic configuration persistence operations.
// All write operations use atomic CAS (WHERE version = expectedVersion) for optimistic locking.
//
// Column name convention: GORM raw strings are used in WHERE clauses throughout this repo,
// matching the column tags in model.ServerConfig. Key columns:
//   - "key"      -> ServerConfig.Key (primaryKey)
//   - "user_id"  -> ServerConfig.UserID (primaryKey, nullable; NULL = system config)
//   - "version"  -> ServerConfig.Version (optimistic lock counter)
type ConfigRepo interface {
	// Get returns a single config by key and user_id.
	// userID=nil queries system config (user_id IS NULL).
	// Returns ErrNotFound if the record does not exist.
	Get(ctx context.Context, key string, userID *uuid.UUID) (*model.ServerConfig, error)

	// List returns configs matching the filter.
	List(ctx context.Context, filter model.ConfigFilter) ([]*model.ServerConfig, error)

	// Upsert performs an atomic CAS update or insert.
	// expectedVersion=0 means INSERT (first write); expectedVersion>0 means UPDATE with WHERE version=expectedVersion.
	// expectedVersion<0 means unconditional UPDATE (force overwrite, skip version check); falls back to INSERT if record doesn't exist.
	// Returns ErrConflict if the version does not match (optimistic lock conflict).
	Upsert(ctx context.Context, cfg *model.ServerConfig, expectedVersion int) error

	// Delete performs an atomic CAS delete.
	// Returns the deleted config (for history/audit).
	// Returns ErrNotFound if the record does not exist, ErrConflict if the version does not match.
	Delete(ctx context.Context, key string, userID *uuid.UUID, expectedVersion int) (*model.ServerConfig, error)

	// ListHistory returns config change history ordered by changed_at DESC.
	ListHistory(ctx context.Context, filter model.ConfigHistoryFilter, page, pageSize int) ([]*model.ServerConfigHistory, int64, error)

	// GetHistoryByVersion returns a single history entry by key, user_id, and version.
	// Returns ErrNotFound if not found.
	GetHistoryByVersion(ctx context.Context, key string, userID *uuid.UUID, version int) (*model.ServerConfigHistory, error)

	// TrimHistory removes the oldest history entries for a key+user_id combo when count exceeds maxCount.
	TrimHistory(ctx context.Context, key string, userID *uuid.UUID, maxCount int) error

	// DeleteByUserID deletes all config overrides for a given user_id.
	// Returns the deleted configs (for audit/cascade).
	// Reserved for future user deletion workflows (e.g., GDPR erasure, account cleanup).
	// Currently not called by any usecase; kept to avoid re-implementation when needed.
	DeleteByUserID(ctx context.Context, userID uuid.UUID) ([]*model.ServerConfig, error)
}

type configRepo struct {
	db *gorm.DB
}

// NewConfigRepo creates a new ConfigRepo.
func NewConfigRepo(db *gorm.DB) ConfigRepo {
	return &configRepo{db: db}
}

func (r *configRepo) Get(ctx context.Context, key string, userID *uuid.UUID) (*model.ServerConfig, error) {
	db := DBFromContext(ctx, r.db).WithContext(ctx)
	var cfg model.ServerConfig
	query := db.Where("key = ?", key)
	if userID == nil {
		query = query.Where("user_id IS NULL")
	} else {
		query = query.Where("user_id = ?", *userID)
	}
	if err := query.First(&cfg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get config %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("get config %q: %w", key, err)
	}
	return &cfg, nil
}

func (r *configRepo) List(ctx context.Context, filter model.ConfigFilter) ([]*model.ServerConfig, error) {
	db := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.ServerConfig{})
	if filter.Category != "" {
		db = db.Where("category = ?", filter.Category)
	}
	if filter.UserID == nil {
		db = db.Where("user_id IS NULL")
	} else {
		db = db.Where("user_id = ?", *filter.UserID)
	}
	var configs []*model.ServerConfig
	if err := db.Find(&configs).Error; err != nil {
		return nil, fmt.Errorf("list configs: %w", err)
	}
	return configs, nil
}

func (r *configRepo) Upsert(ctx context.Context, cfg *model.ServerConfig, expectedVersion int) error {
	db := DBFromContext(ctx, r.db)

	if expectedVersion < 0 {
		// Force overwrite: unconditional UPDATE, no CAS.
		// Usecase has already resolved the correct new version.
		query := db.WithContext(ctx).Model(&model.ServerConfig{}).
			Where("key = ?", cfg.Key)
		if cfg.UserID == nil {
			query = query.Where("user_id IS NULL")
		} else {
			query = query.Where("user_id = ?", *cfg.UserID)
		}
		result := query.Updates(map[string]interface{}{
			"value":       cfg.Value,
			"value_type":  cfg.ValueType,
			"category":    cfg.Category,
			"description": cfg.Description,
			"version":     cfg.Version,
			"updated_by":  cfg.UpdatedBy,
			"updated_at":  cfg.UpdatedAt,
		})
		if result.Error != nil {
			return fmt.Errorf("upsert config %q: %w", cfg.Key, result.Error)
		}
		if result.RowsAffected == 0 {
			// Record doesn't exist; fall back to INSERT.
			if err := db.WithContext(ctx).Create(cfg).Error; err != nil {
				if IsDuplicateKeyError(err) {
					return fmt.Errorf("upsert config %q: %w", cfg.Key, ErrConflict)
				}
				return fmt.Errorf("upsert config %q: %w", cfg.Key, err)
			}
		}
		return nil
	}

	if expectedVersion == 0 {
		// First write: INSERT. Primary key conflict → optimistic lock conflict.
		if err := db.WithContext(ctx).Create(cfg).Error; err != nil {
			if IsDuplicateKeyError(err) {
				return fmt.Errorf("upsert config %q: %w", cfg.Key, ErrConflict)
			}
			return fmt.Errorf("upsert config %q: %w", cfg.Key, err)
		}
		return nil
	}

	// Subsequent update: atomic CAS with version check.
	query := db.WithContext(ctx).Model(&model.ServerConfig{}).
		Where("key = ? AND version = ?", cfg.Key, expectedVersion)
	if cfg.UserID == nil {
		query = query.Where("user_id IS NULL")
	} else {
		query = query.Where("user_id = ?", *cfg.UserID)
	}
	result := query.Updates(map[string]interface{}{
		"value":       cfg.Value,
		"value_type":  cfg.ValueType,
		"category":    cfg.Category,
		"description": cfg.Description,
		"version":     cfg.Version,
		"updated_by":  cfg.UpdatedBy,
		"updated_at":  cfg.UpdatedAt,
	})
	if result.Error != nil {
		return fmt.Errorf("upsert config %q: %w", cfg.Key, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("upsert config %q: %w", cfg.Key, ErrConflict)
	}
	return nil
}

// Delete performs an atomic CAS delete.
// Returns the deleted config (for history/audit).
// Returns ErrNotFound if the record does not exist, ErrConflict if the version does not match.
// WHERE clause uses raw column names (see ConfigRepo interface doc for column mapping).
func (r *configRepo) Delete(ctx context.Context, key string, userID *uuid.UUID, expectedVersion int) (*model.ServerConfig, error) {
	db := DBFromContext(ctx, r.db)

	// Step 1: Check if the record exists (without version check) to distinguish
	// "not found" from "version mismatch".
	existQuery := db.WithContext(ctx).Where("key = ?", key)
	if userID == nil {
		existQuery = existQuery.Where("user_id IS NULL")
	} else {
		existQuery = existQuery.Where("user_id = ?", *userID)
	}
	var existing model.ServerConfig
	if err := existQuery.First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("delete config %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("delete config %q: %w", key, err)
	}

	// Step 2: Atomic CAS delete with version check.
	delQuery := db.WithContext(ctx).Where("key = ? AND version = ?", key, expectedVersion)
	if userID == nil {
		delQuery = delQuery.Where("user_id IS NULL")
	} else {
		delQuery = delQuery.Where("user_id = ?", *userID)
	}
	result := delQuery.Delete(&model.ServerConfig{})
	if result.Error != nil {
		return nil, fmt.Errorf("delete config %q: %w", key, result.Error)
	}
	if result.RowsAffected == 0 {
		// Record exists but version does not match.
		return nil, fmt.Errorf("delete config %q: %w", key, ErrConflict)
	}
	return &existing, nil
}

func (r *configRepo) ListHistory(ctx context.Context, filter model.ConfigHistoryFilter, page, pageSize int) ([]*model.ServerConfigHistory, int64, error) {
	db := DBFromContext(ctx, r.db).WithContext(ctx).Model(&model.ServerConfigHistory{})
	db = db.Where("key = ?", filter.Key)
	if filter.UserID == nil {
		db = db.Where("user_id IS NULL")
	} else {
		db = db.Where("user_id = ?", *filter.UserID)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count config history: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	var histories []*model.ServerConfigHistory
	if err := db.Order("changed_at DESC").Offset(offset).Limit(pageSize).Find(&histories).Error; err != nil {
		return nil, 0, fmt.Errorf("list config history: %w", err)
	}
	return histories, total, nil
}

func (r *configRepo) GetHistoryByVersion(ctx context.Context, key string, userID *uuid.UUID, version int) (*model.ServerConfigHistory, error) {
	db := DBFromContext(ctx, r.db).WithContext(ctx)
	query := db.Where("key = ? AND version = ?", key, version)
	if userID == nil {
		query = query.Where("user_id IS NULL")
	} else {
		query = query.Where("user_id = ?", *userID)
	}
	var h model.ServerConfigHistory
	if err := query.First(&h).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get config history %q v%d: %w", key, version, ErrNotFound)
		}
		return nil, fmt.Errorf("get config history %q v%d: %w", key, version, err)
	}
	return &h, nil
}

func (r *configRepo) TrimHistory(ctx context.Context, key string, userID *uuid.UUID, maxCount int) error {
	db := DBFromContext(ctx, r.db).WithContext(ctx)

	// Count existing entries.
	countQuery := db.Model(&model.ServerConfigHistory{}).Where("key = ?", key)
	if userID == nil {
		countQuery = countQuery.Where("user_id IS NULL")
	} else {
		countQuery = countQuery.Where("user_id = ?", *userID)
	}
	var total int64
	if err := countQuery.Count(&total).Error; err != nil {
		return fmt.Errorf("trim history count: %w", err)
	}
	if total <= int64(maxCount) {
		return nil
	}

	// Delete the oldest entries beyond maxCount.
	excess := int(total) - maxCount
	subQuery := db.Model(&model.ServerConfigHistory{}).
		Select("id").
		Where("key = ?", key)
	if userID == nil {
		subQuery = subQuery.Where("user_id IS NULL")
	} else {
		subQuery = subQuery.Where("user_id = ?", *userID)
	}
	subQuery = subQuery.Order("changed_at ASC").Limit(excess)

	result := db.Where("id IN (?)", subQuery).Delete(&model.ServerConfigHistory{})
	if result.Error != nil {
		return fmt.Errorf("trim history delete: %w", result.Error)
	}
	return nil
}

// DeleteByUserID deletes all config overrides for a given user_id.
// Returns the deleted configs (for audit/cascade).
// Reserved for future user deletion workflows (e.g., GDPR erasure, account cleanup).
// Uses a transaction to ensure the fetch and delete are atomic, preventing a concurrent
// write between the two steps from producing inconsistent audit data.
func (r *configRepo) DeleteByUserID(ctx context.Context, userID uuid.UUID) ([]*model.ServerConfig, error) {
	db := DBFromContext(ctx, r.db)

	var configs []*model.ServerConfig
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Fetch all user configs before deletion (for audit) inside the transaction.
		if err := tx.Where("user_id = ?", userID).Find(&configs).Error; err != nil {
			return fmt.Errorf("delete by user_id fetch: %w", err)
		}
		if len(configs) == 0 {
			return nil
		}

		// Delete all within the same transaction.
		if err := tx.Where("user_id = ?", userID).Delete(&model.ServerConfig{}).Error; err != nil {
			return fmt.Errorf("delete by user_id: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return configs, nil
}
