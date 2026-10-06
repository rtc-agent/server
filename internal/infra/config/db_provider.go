package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/rtc-agent/server/internal/repo"
)

// DBConfigProvider is the V1 ConfigProvider implementation.
// It reads from the database on every call (no caching).
type DBConfigProvider struct {
	configRepo repo.ConfigRepo
}

// NewDBConfigProvider creates a new DBConfigProvider.
func NewDBConfigProvider(configRepo repo.ConfigRepo) ConfigProvider {
	return &DBConfigProvider{configRepo: configRepo}
}

// GetEffectiveString returns the effective string value for the given config key.
func (p *DBConfigProvider) GetEffectiveString(ctx context.Context, key string, userID *uuid.UUID) (string, error) {
	val, err := p.getEffectiveAny(ctx, key, userID)
	if err != nil {
		return "", err
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("config %q is not a string, got %T", key, val)
	}
	return s, nil
}

// GetEffectiveInt returns the effective integer value for the given config key.
func (p *DBConfigProvider) GetEffectiveInt(ctx context.Context, key string, userID *uuid.UUID) (int, error) {
	val, err := p.getEffectiveAny(ctx, key, userID)
	if err != nil {
		return 0, err
	}
	// JSON unmarshaling may produce float64 for numbers.
	switch v := val.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("config %q: %w", key, err)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("config %q is not an int, got %T", key, val)
	}
}

// GetEffectiveFloat returns the effective float value for the given config key.
func (p *DBConfigProvider) GetEffectiveFloat(ctx context.Context, key string, userID *uuid.UUID) (float64, error) {
	val, err := p.getEffectiveAny(ctx, key, userID)
	if err != nil {
		return 0, err
	}
	switch v := val.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("config %q: %w", key, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("config %q is not a float, got %T", key, val)
	}
}

// GetEffectiveBool returns the effective boolean value for the given config key.
func (p *DBConfigProvider) GetEffectiveBool(ctx context.Context, key string, userID *uuid.UUID) (bool, error) {
	val, err := p.getEffectiveAny(ctx, key, userID)
	if err != nil {
		return false, err
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("config %q is not a bool, got %T", key, val)
	}
	return b, nil
}

// GetEffectiveDuration returns the effective duration value for the given config key.
func (p *DBConfigProvider) GetEffectiveDuration(ctx context.Context, key string, userID *uuid.UUID) (time.Duration, error) {
	s, err := p.GetEffectiveString(ctx, key, userID)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config %q: invalid duration %q: %w", key, s, err)
	}
	return d, nil
}

// GetEffectiveJSON returns the effective JSON value (as any) for the given config key.
func (p *DBConfigProvider) GetEffectiveJSON(ctx context.Context, key string, userID *uuid.UUID) (any, error) {
	return p.getEffectiveAny(ctx, key, userID)
}

// getEffectiveAny implements the three-tier resolution logic:
// user override > system default (DB) > yaml baseline.
func (p *DBConfigProvider) getEffectiveAny(ctx context.Context, key string, userID *uuid.UUID) (any, error) {
	entry := GetRegistryEntry(key)
	if entry == nil {
		return nil, fmt.Errorf("unknown config key: %s", key)
	}

	// Try user override first.
	if userID != nil {
		cfg, err := p.configRepo.Get(ctx, key, userID)
		if err == nil {
			return unmarshalJSONB(cfg.Value), nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return nil, fmt.Errorf("get user config %q: %w", key, err)
		}
	}

	// Try system default.
	cfg, err := p.configRepo.Get(ctx, key, nil)
	if err == nil {
		return unmarshalJSONB(cfg.Value), nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("get system config %q: %w", key, err)
	}

	// Fall back to yaml baseline.
	return entry.YamlDefault, nil
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
