package config

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

// CachedConfigProvider wraps a ConfigProvider with an in-memory cache layer.
// Cache entries expire after a configurable TTL, and can be invalidated via
// InvalidateCache when config updates are received from Redis Pub/Sub.
type CachedConfigProvider struct {
	inner       ConfigProvider
	cache       map[string]*cachedEntry
	cacheMu     sync.RWMutex
	ttl         time.Duration
	flightGroup singleflight.Group // Prevents thundering herd on cache miss
}

type cachedEntry struct {
	value     any
	expiresAt time.Time
}

// NewCachedConfigProvider creates a CachedConfigProvider wrapping the given provider.
// The ttl parameter controls how long config values are cached before re-reading from DB.
// Recommended TTL: 30-60 seconds for most use cases.
func NewCachedConfigProvider(inner ConfigProvider, ttl time.Duration) *CachedConfigProvider {
	return &CachedConfigProvider{
		inner: inner,
		cache: make(map[string]*cachedEntry),
		ttl:   ttl,
	}
}

// GetEffectiveString returns the effective string value, using cache when available.
func (p *CachedConfigProvider) GetEffectiveString(ctx context.Context, key string, userID *uuid.UUID) (string, error) {
	val, err := p.getEffectiveCached(ctx, key, userID, func() (any, error) {
		return p.inner.GetEffectiveString(ctx, key, userID)
	})
	if err != nil {
		return "", err
	}
	s, ok := val.(string)
	if !ok {
		return "", err
	}
	return s, nil
}

// GetEffectiveInt returns the effective int value, using cache when available.
func (p *CachedConfigProvider) GetEffectiveInt(ctx context.Context, key string, userID *uuid.UUID) (int, error) {
	val, err := p.getEffectiveCached(ctx, key, userID, func() (any, error) {
		return p.inner.GetEffectiveInt(ctx, key, userID)
	})
	if err != nil {
		return 0, err
	}
	// JSON unmarshaling may produce float64 for numbers.
	switch v := val.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	default:
		// Re-fetch from inner provider for type conversion.
		return p.inner.GetEffectiveInt(ctx, key, userID)
	}
}

// GetEffectiveFloat returns the effective float64 value, using cache when available.
func (p *CachedConfigProvider) GetEffectiveFloat(ctx context.Context, key string, userID *uuid.UUID) (float64, error) {
	val, err := p.getEffectiveCached(ctx, key, userID, func() (any, error) {
		return p.inner.GetEffectiveFloat(ctx, key, userID)
	})
	if err != nil {
		return 0, err
	}
	switch v := val.(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	default:
		return p.inner.GetEffectiveFloat(ctx, key, userID)
	}
}

// GetEffectiveBool returns the effective bool value, using cache when available.
func (p *CachedConfigProvider) GetEffectiveBool(ctx context.Context, key string, userID *uuid.UUID) (bool, error) {
	val, err := p.getEffectiveCached(ctx, key, userID, func() (any, error) {
		return p.inner.GetEffectiveBool(ctx, key, userID)
	})
	if err != nil {
		return false, err
	}
	b, ok := val.(bool)
	if !ok {
		return p.inner.GetEffectiveBool(ctx, key, userID)
	}
	return b, nil
}

// GetEffectiveDuration returns the effective duration value, using cache when available.
func (p *CachedConfigProvider) GetEffectiveDuration(ctx context.Context, key string, userID *uuid.UUID) (time.Duration, error) {
	s, err := p.GetEffectiveString(ctx, key, userID)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return d, nil
}

// GetEffectiveJSON returns the effective JSON value, using cache when available.
func (p *CachedConfigProvider) GetEffectiveJSON(ctx context.Context, key string, userID *uuid.UUID) (any, error) {
	return p.getEffectiveCached(ctx, key, userID, func() (any, error) {
		return p.inner.GetEffectiveJSON(ctx, key, userID)
	})
}

// getEffectiveCached checks the cache for a value, fetching from inner provider if missing or expired.
// Uses singleflight to prevent thundering herd when multiple goroutines request the same key simultaneously.
func (p *CachedConfigProvider) getEffectiveCached(_ context.Context, key string, userID *uuid.UUID, fetch func() (any, error)) (any, error) {
	cacheKey := buildCacheKey(key, userID)

	// Check cache (read lock for performance).
	p.cacheMu.RLock()
	if entry, ok := p.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		p.cacheMu.RUnlock()
		return entry.value, nil
	}
	p.cacheMu.RUnlock()

	// Cache miss or expired - use singleflight to deduplicate concurrent fetches.
	// This prevents thundering herd when many goroutines simultaneously request the same missing/expired key.
	result, err, _ := p.flightGroup.Do(cacheKey, func() (any, error) {
		// Double-check cache inside singleflight to handle race conditions.
		// Another goroutine may have populated the cache while we were waiting.
		p.cacheMu.RLock()
		if entry, ok := p.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
			p.cacheMu.RUnlock()
			return entry.value, nil
		}
		p.cacheMu.RUnlock()

		// Fetch from inner provider.
		value, err := fetch()
		if err != nil {
			return nil, err
		}

		// Store in cache (write lock).
		p.cacheMu.Lock()
		p.cache[cacheKey] = &cachedEntry{
			value:     value,
			expiresAt: time.Now().Add(p.ttl),
		}
		p.cacheMu.Unlock()

		return value, nil
	})

	if err != nil {
		return nil, err
	}

	return result, nil
}

// InvalidateCache removes a specific key from the cache.
// Called when a config update event is received from Redis Pub/Sub.
// When userID is nil (system config update), all user-level entries for that key
// are also invalidated to prevent stale fallback values (P0-3 fix).
func (p *CachedConfigProvider) InvalidateCache(key string, userID *uuid.UUID) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if userID == nil {
		// System config update: invalidate system entry and all user-level entries for this key.
		// User cache keys have the format "user:{userID}:{key}".
		systemCacheKey := buildCacheKey(key, nil)
		delete(p.cache, systemCacheKey)

		// Iterate and remove all user-level entries for this key.
		// This is necessary because user effective values may have been resolved from
		// the old system config (fallback), and those cached values are now stale.
		userKeyPrefix := "user:"
		userKeySuffix := ":" + key
		for cacheKey := range p.cache {
			if len(cacheKey) > len(userKeyPrefix)+len(userKeySuffix) &&
				cacheKey[:len(userKeyPrefix)] == userKeyPrefix &&
				cacheKey[len(cacheKey)-len(userKeySuffix):] == userKeySuffix {
				delete(p.cache, cacheKey)
			}
		}
	} else {
		// User-specific config update: invalidate only that user's entry.
		cacheKey := buildCacheKey(key, userID)
		delete(p.cache, cacheKey)
	}
}

// InvalidateAll clears the entire cache.
// Called on startup or when a global config change is detected.
func (p *CachedConfigProvider) InvalidateAll() {
	p.cacheMu.Lock()
	p.cache = make(map[string]*cachedEntry)
	p.cacheMu.Unlock()
}

// buildCacheKey creates a unique cache key from config key and user ID.
// System configs (nil userID) use a special prefix to distinguish from user configs.
func buildCacheKey(key string, userID *uuid.UUID) string {
	if userID == nil {
		return "system:" + key
	}
	return "user:" + userID.String() + ":" + key
}
