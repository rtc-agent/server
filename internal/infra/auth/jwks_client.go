// Package auth provides JWT signing, verification, and JWKS key management.
//
// The JWKSClient fetches and caches JSON Web Key Sets from external issuers,
// enabling RFC 8693 Token Exchange verification of external JWTs.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/pkg/logger"
)

// Sentinel errors for JWKS operations.
var (
	// ErrJWKSEndpointUnreachable indicates the JWKS endpoint could not be reached.
	// Callers should return HTTP 503 to the client.
	ErrJWKSEndpointUnreachable = errors.New("jwks endpoint unreachable")

	// ErrJWKSKeyNotFound indicates the requested key ID was not found in the JWKS.
	// Callers should return HTTP 401 to the client.
	ErrJWKSKeyNotFound = errors.New("jwks key not found")

	// ErrJWKSInvalidResponse indicates the JWKS response was malformed.
	ErrJWKSInvalidResponse = errors.New("jwks invalid response")
)

// safeReleaseLua is a Lua script for safe distributed lock release.
// Only deletes the key if the current holder matches, preventing one client
// from accidentally releasing another client's lock.
const safeReleaseLua = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

// JWKSClientConfig holds configuration for a JWKS client.
type JWKSClientConfig struct {
	// HTTPClient is the HTTP client used for fetching JWKS. Defaults to a 10s timeout client.
	HTTPClient *http.Client
	// RedisClient is the Redis client for caching. Required.
	RedisClient redis.UniversalClient
	// DefaultCacheTTL is the default cache TTL for JWKS keys. Defaults to 1 hour.
	DefaultCacheTTL time.Duration
	// LockTTL is the distributed lock TTL for preventing concurrent refresh. Defaults to 10s.
	LockTTL time.Duration
	// MaxResponseSize is the maximum JWKS response size in bytes. Defaults to 1MB.
	MaxResponseSize int64
}

// JWKSClient fetches, caches, and manages JWKS public keys from external issuers.
//
// Caching strategy:
//   - Keys are cached in Redis with key format "jwks:{issuer}:{kid}"
//   - On cache miss, the full JWKS is fetched from the issuer's jwks_uri
//   - On verification failure, the cache is automatically refreshed
//   - A distributed lock prevents multiple instances from simultaneously refreshing
type JWKSClient struct {
	httpClient      *http.Client
	redis           redis.UniversalClient
	defaultCacheTTL time.Duration
	lockTTL         time.Duration
	maxResponseSize int64
}

// NewJWKSClient creates a new JWKSClient with the given configuration.
func NewJWKSClient(cfg JWKSClientConfig) (*JWKSClient, error) {
	if cfg.RedisClient == nil {
		return nil, errors.New("jwks client: redis client is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	cacheTTL := cfg.DefaultCacheTTL
	if cacheTTL <= 0 {
		cacheTTL = time.Hour
	}
	lockTTL := cfg.LockTTL
	if lockTTL <= 0 {
		lockTTL = 10 * time.Second
	}
	maxSize := cfg.MaxResponseSize
	if maxSize <= 0 {
		maxSize = 1 << 20 // 1MB
	}
	return &JWKSClient{
		httpClient:      httpClient,
		redis:           cfg.RedisClient,
		defaultCacheTTL: cacheTTL,
		lockTTL:         lockTTL,
		maxResponseSize: maxSize,
	}, nil
}

// FetchJWKS fetches the JWKS from the given URI and returns the key set.
// Keys are cached in Redis; on cache miss, the full JWKS is fetched.
//
// Security: HTTPS is required for external hosts. HTTP is allowed for:
// - localhost, 127.0.0.1
// - .local, .orb.local domains
// - Docker internal service names (hostnames without dots)
// Response size is capped at MaxResponseSize.
func (c *JWKSClient) FetchJWKS(ctx context.Context, jwksURI string) (jwk.Set, error) {
	// Security: enforce HTTPS for external hosts to prevent MITM cache poisoning.
	// Allow HTTP for local/Docker internal services in development.
	if !strings.HasPrefix(jwksURI, "https://") {
		u, err := url.Parse(jwksURI)
		if err != nil {
			return nil, fmt.Errorf("invalid jwks URI: %w", err)
		}
		if u.Scheme != "http" {
			return nil, fmt.Errorf("jwks URI must use HTTP or HTTPS: %s", jwksURI)
		}
		// Allow HTTP only for local/Docker internal services
		host := u.Hostname()
		isLocal := host == "localhost" || host == "127.0.0.1" ||
			strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".orb.local")
		hasNoDots := !strings.Contains(host, ".") // Docker internal services
		if !isLocal && !hasNoDots {
			return nil, fmt.Errorf("jwks URI must use HTTPS for external hosts: %s", jwksURI)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, fmt.Errorf("build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJWKSEndpointUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrJWKSEndpointUnreachable, resp.StatusCode)
	}

	// Validate Content-Type to prevent response smuggling.
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil, fmt.Errorf("%w: unexpected content-type %q", ErrJWKSInvalidResponse, ct)
	}

	// Limit response size to prevent memory exhaustion.
	limitedReader := io.LimitReader(resp.Body, c.maxResponseSize)

	// Read all data
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrJWKSInvalidResponse, err)
	}

	set, err := jwk.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJWKSInvalidResponse, err)
	}

	return set, nil
}

// GetKey retrieves a specific key by issuer and key ID.
// It first checks the Redis cache, then falls back to fetching the full JWKS.
func (c *JWKSClient) GetKey(ctx context.Context, issuer, jwksURI, kid string) (jwk.Key, error) {
	// Try cache first.
	cachedKey, err := c.getCachedKey(ctx, issuer, kid)
	if err == nil && cachedKey != nil {
		return cachedKey, nil
	}

	// Cache miss: fetch full JWKS and cache all keys.
	set, err := c.FetchJWKS(ctx, jwksURI)
	if err != nil {
		return nil, err
	}

	// Cache all keys from the JWKS.
	c.cacheKeySet(ctx, issuer, set)

	// Look up the specific key.
	key, found := set.LookupKeyID(kid)
	if !found {
		return nil, fmt.Errorf("%w: kid=%s issuer=%s", ErrJWKSKeyNotFound, kid, issuer)
	}

	return key, nil
}

// RefreshKey forces a cache refresh for the given issuer's JWKS.
// Uses a distributed lock to prevent concurrent refreshes across instances.
func (c *JWKSClient) RefreshKey(ctx context.Context, issuer, jwksURI, kid string) (jwk.Key, error) {
	// Acquire distributed lock to prevent concurrent refreshes.
	lockKey := cache.JWKSLockKey(issuer)
	holderUUID := uuid.Must(uuid.NewV7()).String()

	// SETNX with lock TTL.
	acquired, err := c.redis.SetNX(ctx, lockKey, holderUUID, c.lockTTL).Result()
	if err != nil {
		logger.Warn(ctx, "jwks: failed to acquire refresh lock",
			zap.String("issuer", issuer), zap.Error(err))
		// Fall through to fetch anyway; lock failure should not block verification.
	}

	if !acquired {
		// Another instance is refreshing; wait briefly and try cache again.
		// Use a context-aware timer so the wait can be cancelled on shutdown.
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		cachedKey, cacheErr := c.getCachedKey(ctx, issuer, kid)
		if cacheErr == nil && cachedKey != nil {
			return cachedKey, nil
		}
		// Lock holder may have failed; proceed with fetch anyway.
	}

	// Release the distributed lock after we're done (only if we successfully acquired it).
	// The Lua script ensures we only delete our own lock, never another instance's.
	if acquired {
		defer func() {
			_ = c.redis.Eval(ctx, safeReleaseLua, []string{lockKey}, holderUUID).Err()
		}()
	}

	// Fetch fresh JWKS.
	set, err := c.FetchJWKS(ctx, jwksURI)
	if err != nil {
		return nil, err
	}

	// Cache all keys.
	c.cacheKeySet(ctx, issuer, set)

	key, found := set.LookupKeyID(kid)
	if !found {
		return nil, fmt.Errorf("%w: kid=%s issuer=%s", ErrJWKSKeyNotFound, kid, issuer)
	}

	return key, nil
}

// getCachedKey retrieves a cached JWK from Redis.
func (c *JWKSClient) getCachedKey(ctx context.Context, issuer, kid string) (jwk.Key, error) {
	cacheKey := cache.JWKSKey(issuer, kid)
	data, err := c.redis.Get(ctx, cacheKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("redis get jwks key: %w", err)
	}

	key, err := jwk.ParseKey([]byte(data))
	if err != nil {
		// Corrupted cache entry; delete it.
		_ = c.redis.Del(ctx, cacheKey).Err()
		return nil, fmt.Errorf("parse cached jwk: %w", err)
	}

	return key, nil
}

// cacheKeySet stores all keys from a JWKS into Redis.
// Individual key caching failures are logged but do not fail the whole operation.
func (c *JWKSClient) cacheKeySet(ctx context.Context, issuer string, set jwk.Set) {
	for iter := set.Keys(ctx); iter.Next(ctx); {
		pair := iter.Pair()
		key, ok := pair.Value.(jwk.Key)
		if !ok {
			continue
		}
		kid := key.KeyID()
		if kid == "" {
			continue
		}

		// Serialize the key to JSON for caching.
		jsonBytes, err := json.Marshal(key)
		if err != nil {
			logger.Warn(ctx, "jwks: failed to serialize key",
				zap.String("kid", kid), zap.Error(err))
			continue
		}

		cacheKey := cache.JWKSKey(issuer, kid)
		if err := c.redis.Set(ctx, cacheKey, jsonBytes, c.defaultCacheTTL).Err(); err != nil {
			logger.Warn(ctx, "jwks: failed to cache key",
				zap.String("kid", kid), zap.Error(err))
		}
	}
}
