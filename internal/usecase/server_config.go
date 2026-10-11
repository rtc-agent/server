// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/repo"
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
	configRepo      repo.ServerConfigRepo
	auditLogRepo    repo.AuditLogRepo
	oauth2UserRepo  repo.OAuth2UserRepo
	db              *gorm.DB
	configPublisher ConfigPublisher // optional: publishes config update events for multi-instance cache sync
}

// ConfigPublisher publishes config update events (implemented by RedisConfigPublisher).
// This interface allows the usecase to publish events without directly depending on Redis.
type ConfigPublisher interface {
	PublishConfigUpdate(ctx context.Context, key string, userID *uuid.UUID, version int, operation string) error
}

// NewServerConfigUsecase creates a new ServerConfigUsecase.
func NewServerConfigUsecase(configRepo repo.ServerConfigRepo, auditLogRepo repo.AuditLogRepo, oauth2UserRepo repo.OAuth2UserRepo, db *gorm.DB) *ServerConfigUsecase {
	return &ServerConfigUsecase{
		configRepo:     configRepo,
		auditLogRepo:   auditLogRepo,
		oauth2UserRepo: oauth2UserRepo,
		db:             db,
	}
}

// SetConfigPublisher sets the optional config publisher for multi-instance cache synchronization.
// When set, config updates will be published to Redis Pub/Sub, allowing other instances
// to invalidate their caches and maintain consistency.
func (uc *ServerConfigUsecase) SetConfigPublisher(publisher ConfigPublisher) {
	uc.configPublisher = publisher
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

// ── Helpers ──────────────────────────────────────────────────────────

// getOldValue fetches the current JSON value for history tracking.
// Returns yaml default value (as JSON) if DB has no record, ensuring v1 history
// shows the transition from yaml baseline to DB value.
// Should be called within a transaction to ensure atomicity with the write operation (P2-2 fix).
func (uc *ServerConfigUsecase) getOldValue(ctx context.Context, key string, userID *uuid.UUID, entry *config.ConfigEntry) datatypes.JSON {
	cfg, err := uc.configRepo.Get(ctx, key, userID)
	if err != nil {
		// DB has no record; return yaml default as JSON for history completeness.
		if entry != nil && entry.YamlDefault != nil {
			jsonVal, marshalErr := json.Marshal(entry.YamlDefault)
			if marshalErr == nil {
				return datatypes.JSON(jsonVal)
			}
		}
		return nil
	}
	return cfg.Value
}

// marshalAndValidateValue marshals a value to JSON and validates it against the registry entry.
func marshalAndValidateValue(value any, entry *config.ConfigEntry) ([]byte, error) {
	// Type-specific conversion and validation.
	converted, err := convertValueByType(value, entry.ValueType)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValidation, err)
	}

	// Run custom validator if present.
	if entry.Validator != nil {
		if vErr := entry.Validator(converted); vErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, vErr)
		}
	}

	jsonVal, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal value: %w", ErrValidation, err)
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
		// Check for decimal part to prevent silent truncation
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("float64 has decimal part, cannot convert to int: %v", n)
		}
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

func userIDString(uid *uuid.UUID) any {
	if uid == nil {
		return nil
	}
	return uid.String()
}

// paginateItems applies page/pageSize pagination to a slice.
// Returns an empty slice if the page is out of range.
func paginateItems[T any](items []T, page, pageSize int) []T {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	if offset >= len(items) {
		return []T{}
	}
	end := offset + pageSize
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}
