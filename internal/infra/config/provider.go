// Package config provides the dynamic configuration provider interface.
//
// ConfigProvider abstracts the reading of dynamic configuration values,
// allowing for easy extension (e.g., adding caching in V2).
package config

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ConfigProvider abstracts dynamic configuration reading.
// V1 implementation reads from DB on every call (no caching).
// V2 can wrap a DBConfigProvider with a caching layer.
type ConfigProvider interface {
	// GetEffectiveString returns the effective string value for the given config key.
	// Resolution order: user override > system default (DB) > yaml baseline.
	GetEffectiveString(ctx context.Context, key string, userID *uuid.UUID) (string, error)

	// GetEffectiveInt returns the effective int value for the given config key.
	GetEffectiveInt(ctx context.Context, key string, userID *uuid.UUID) (int, error)

	// GetEffectiveFloat returns the effective float64 value for the given config key.
	GetEffectiveFloat(ctx context.Context, key string, userID *uuid.UUID) (float64, error)

	// GetEffectiveBool returns the effective bool value for the given config key.
	GetEffectiveBool(ctx context.Context, key string, userID *uuid.UUID) (bool, error)

	// GetEffectiveDuration returns the effective duration value for the given config key.
	GetEffectiveDuration(ctx context.Context, key string, userID *uuid.UUID) (time.Duration, error)

	// GetEffectiveJSON returns the effective JSON value (as Go any type) for the given config key.
	GetEffectiveJSON(ctx context.Context, key string, userID *uuid.UUID) (any, error)
}
