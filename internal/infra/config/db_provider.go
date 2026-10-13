package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// circuitBreakerGauge tracks the circuit breaker state for DB config provider.
// 0 = closed (normal operation), 1 = open (degraded mode, falling back to yaml).
var circuitBreakerGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "rtc",
	Subsystem: "config_db",
	Name:      "circuit_breaker_state",
	Help:      "Circuit breaker state for DB config provider (0=closed/healthy, 1=open/degraded).",
})

// DBConfigProvider is the V1 ConfigProvider implementation.
// It reads from the database on every call (no caching).
type DBConfigProvider struct {
	configRepo repo.ServerConfigRepo
	metrics    *turnagent.PrometheusMetrics

	// Circuit breaker state for DB degradation
	consecutiveErrors int
	circuitOpen       bool
	lastErrorTime     time.Time
	mu                sync.Mutex

	// Fallback sampling: log only every Nth fallback to reduce log volume
	fallbackCount     int
	fallbackLogSample int // Log every Nth fallback (e.g., 10 means log 1 in 10)
}

const (
	circuitThreshold = 3                // consecutive errors before opening circuit
	circuitCooldown  = 30 * time.Second // cooldown period before attempting to close circuit
)

// NewDBConfigProvider creates a new DBConfigProvider.
func NewDBConfigProvider(configRepo repo.ServerConfigRepo, metrics *turnagent.PrometheusMetrics) ConfigProvider {
	return &DBConfigProvider{
		configRepo:        configRepo,
		metrics:           metrics,
		fallbackLogSample: 10, // Log every 10th fallback to reduce alert fatigue
	}
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
// When DB is unavailable (circuit open), it degrades to yaml baseline directly.
func (p *DBConfigProvider) getEffectiveAny(ctx context.Context, key string, userID *uuid.UUID) (any, error) {
	entry := GetRegistryEntry(key)
	if entry == nil {
		return nil, fmt.Errorf("unknown config key: %s", key)
	}

	// Check circuit breaker: if open, degrade to yaml baseline
	if p.isCircuitOpen() {
		if p.metrics != nil {
			p.metrics.RecordConfigFallback(ctx, key, "circuit_open")
		}
		return entry.YamlDefault, nil
	}

	// Try user override first.
	if userID != nil {
		cfg, err := p.configRepo.Get(ctx, key, userID)
		if err == nil {
			p.resetCircuit() // DB success, reset circuit breaker
			return UnmarshalJSONB(cfg.Value), nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			// DB error: record and potentially open circuit
			p.recordError(ctx, key)
			// Degrade to yaml baseline instead of returning error to caller.
			// This is expected behavior during DB outages (circuit breaker pattern).
			// Use Warn level (not Error) to avoid alert fatigue, with sampling to reduce volume.
			if p.shouldLogFallback() {
				logger.Warn(ctx, "db_config_user_lookup_failed_degrading_to_yaml",
					zap.String("config_key", key),
					zap.String("user_id", userID.String()),
					zap.Error(err))
			}
			if p.metrics != nil {
				p.metrics.RecordConfigFallback(ctx, key, "db_error")
			}
			return entry.YamlDefault, nil
		}
		// User config not found, will fall through to system default.
	}

	// Try system default.
	cfg, err := p.configRepo.Get(ctx, key, nil)
	if err == nil {
		p.resetCircuit() // DB success, reset circuit breaker
		return UnmarshalJSONB(cfg.Value), nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		// DB error: record and potentially open circuit
		p.recordError(ctx, key)
		// Degrade to yaml baseline instead of returning error to caller.
		// This is expected behavior during DB outages (circuit breaker pattern).
		// Use Warn level (not Error) to avoid alert fatigue, with sampling to reduce volume.
		if p.shouldLogFallback() {
			logger.Warn(ctx, "db_config_system_lookup_failed_degrading_to_yaml",
				zap.String("config_key", key),
				zap.Error(err))
		}
		if p.metrics != nil {
			p.metrics.RecordConfigFallback(ctx, key, "db_error")
		}
		return entry.YamlDefault, nil
	}

	// Fall back to yaml baseline: record fallback metric.
	p.resetCircuit() // Not a DB error, reset circuit
	if p.metrics != nil {
		p.metrics.RecordConfigFallback(ctx, key, "not_found")
	}
	return entry.YamlDefault, nil
}

// isCircuitOpen checks if the circuit breaker is open (degraded mode).
// Returns true if circuit is open and still within cooldown period.
func (p *DBConfigProvider) isCircuitOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.circuitOpen {
		return false
	}

	// Check if cooldown period has elapsed
	if time.Since(p.lastErrorTime) > circuitCooldown {
		// Attempt to close circuit by allowing next request through
		p.circuitOpen = false
		p.consecutiveErrors = 0
		return false
	}

	return true
}

// recordError records a DB error and opens circuit if threshold is reached.
func (p *DBConfigProvider) recordError(ctx context.Context, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.consecutiveErrors++
	p.lastErrorTime = time.Now()

	if p.consecutiveErrors >= circuitThreshold && !p.circuitOpen {
		p.circuitOpen = true
		circuitBreakerGauge.Set(1) // Circuit opened
		logger.Warn(ctx, "db_config_circuit_opened",
			zap.String("config_key", key),
			zap.Int("consecutive_errors", p.consecutiveErrors))
	}
}

// resetCircuit resets the circuit breaker state after a successful DB operation.
func (p *DBConfigProvider) resetCircuit() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.consecutiveErrors = 0
	p.circuitOpen = false
	p.fallbackCount = 0        // Reset fallback counter
	circuitBreakerGauge.Set(0) // Circuit closed
}

// shouldLogFallback increments fallback counter and returns true if this fallback should be logged.
// Implements sampling: logs every Nth fallback to reduce log volume during sustained outages.
func (p *DBConfigProvider) shouldLogFallback() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.fallbackCount++
	// Log if:
	// 1. This is the first fallback (always log initial degradation)
	// 2. This is an Nth fallback (sampled logging)
	if p.fallbackCount == 1 || p.fallbackCount%p.fallbackLogSample == 0 {
		return true
	}
	return false
}
