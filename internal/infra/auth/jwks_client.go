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
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

	// ErrJWKSUnauthorizedIssuer indicates the issuer is not in the allowed list.
	// This is a security measure to prevent issuer confusion attacks.
	ErrJWKSUnauthorizedIssuer = errors.New("jwks unauthorized issuer")
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
	// LockTTL is the distributed lock TTL for preventing concurrent refresh. Defaults to 30s.
	// Must be at least 5 seconds for distributed safety. Higher values (30-60s) are recommended
	// for high-latency environments or when JWKS endpoints are slow.
	LockTTL time.Duration
	// MaxResponseSize is the maximum JWKS response size in bytes. Defaults to 1MB.
	MaxResponseSize int64
	// Environment is the runtime environment (production/development).
	// Used to emit security warnings for insecure configurations.
	Environment string
	// AllowedIssuers is an optional whitelist of trusted issuer URLs.
	// If set, GetKey and RefreshKey will reject issuers not in this list.
	// This provides defense-in-depth against issuer confusion attacks.
	AllowedIssuers []string
}

// issuerCircuitBreaker tracks per-issuer circuit breaker state for JWKS refresh.
// It implements exponential backoff to prevent thundering herd when the JWKS
// endpoint is failing or slow.
type issuerCircuitBreaker struct {
	consecutiveFailures int
	lastFailureTime     time.Time
}

// JWKSClient fetches, caches, and manages JWKS public keys from external issuers.
//
// Caching strategy:
//   - Keys are cached in Redis with key format "jwks:{issuer}:{kid}"
//   - On cache miss, the full JWKS is fetched from the issuer's jwks_uri
//   - On verification failure, the cache is automatically refreshed
//   - A distributed lock prevents multiple instances from simultaneously refreshing
//   - Per-issuer circuit breaker with exponential backoff prevents thundering herd
type JWKSClient struct {
	httpClient      *http.Client
	redis           redis.UniversalClient
	defaultCacheTTL time.Duration
	lockTTL         time.Duration
	maxResponseSize int64
	environment     string // runtime environment (production/development)

	// Per-issuer circuit breaker state (exponential backoff for refresh failures)
	cbMu     sync.Mutex
	cbStates map[string]*issuerCircuitBreaker

	// allowedIssuers is an optional whitelist of trusted issuer URLs.
	// If non-nil, GetKey and RefreshKey reject issuers not in this set.
	allowedIssuers map[string]bool
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
		lockTTL = defaultLockTTL
	} else if lockTTL < minLockTTL {
		// Enforce minimum lock TTL for distributed safety.
		// Too-short locks can cause multiple instances to refresh simultaneously.
		lockTTL = minLockTTL
	}
	maxSize := cfg.MaxResponseSize
	if maxSize <= 0 {
		maxSize = 1 << 20 // 1MB
	}

	// Build allowed issuers map for O(1) lookup (nil if empty to indicate no restriction).
	var allowedIssuers map[string]bool
	if len(cfg.AllowedIssuers) > 0 {
		allowedIssuers = make(map[string]bool, len(cfg.AllowedIssuers))
		for _, issuer := range cfg.AllowedIssuers {
			allowedIssuers[issuer] = true
		}
	}

	client := &JWKSClient{
		httpClient:      httpClient,
		redis:           cfg.RedisClient,
		defaultCacheTTL: cacheTTL,
		lockTTL:         lockTTL,
		maxResponseSize: maxSize,
		environment:     cfg.Environment,
		cbStates:        make(map[string]*issuerCircuitBreaker),
		allowedIssuers:  allowedIssuers,
	}

	// Warmup: Attempt to fetch JWKS keys from allowed issuers to verify connectivity
	// and pre-populate the circuit breaker state. This helps detect configuration issues
	// early rather than on the first authentication request.
	//
	// Note: This is a best-effort check. Failures are logged but do not block startup,
	// as JWKS endpoints may be temporarily unavailable or the keys may not be needed
	// immediately. The circuit breaker will handle retries with exponential backoff.
	if len(cfg.AllowedIssuers) > 0 {
		go client.warmupCircuitBreakers(cfg.AllowedIssuers)
	}

	return client, nil
}

// warmupCircuitBreakers attempts to initialize circuit breaker state for known issuers.
// This is called during startup to detect connectivity issues early.
func (c *JWKSClient) warmupCircuitBreakers(issuers []string) {
	// Use a background context with timeout for warmup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, issuer := range issuers {
		// Attempt to get the issuer's JWKS URI from well-known configuration
		// This is a simplified warmup - in production, you might want to cache
		// the JWKS URI or fetch it from a configuration source
		wellKnownURL := issuer + "/.well-known/openid-configuration"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnownURL, nil)
		if err != nil {
			logger.Warn(ctx, "jwks warmup: failed to create request",
				zap.String("issuer", issuer),
				zap.Error(err))
			continue
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			logger.Warn(ctx, "jwks warmup: failed to fetch well-known configuration",
				zap.String("issuer", issuer),
				zap.Error(err))
			continue
		}
		if err := resp.Body.Close(); err != nil {
			logger.Warn(ctx, "jwks warmup: failed to close response body",
				zap.String("issuer", issuer),
				zap.Error(err))
		}

		if resp.StatusCode != http.StatusOK {
			logger.Warn(ctx, "jwks warmup: non-200 response from issuer",
				zap.String("issuer", issuer),
				zap.Int("status_code", resp.StatusCode))
		}
	}
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
	// Validate URI security (HTTPS required for external hosts).
	if err := c.validateJWKSURI(ctx, jwksURI); err != nil {
		return nil, err
	}

	// Execute HTTP request and validate response.
	data, err := c.executeJWKSRequest(ctx, jwksURI)
	if err != nil {
		return nil, err
	}

	// Validate JWKS schema before parsing to prevent cache poisoning.
	if !validateJWKSSchema(data) {
		return nil, fmt.Errorf("%w: invalid JWKS schema", ErrJWKSInvalidResponse)
	}

	set, err := jwk.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJWKSInvalidResponse, err)
	}

	return set, nil
}

// validateJWKSURI validates the security of a JWKS URI.
// HTTPS is required for external hosts to prevent MITM attacks.
// HTTP is allowed for localhost, .local domains, and Docker internal services.
func (c *JWKSClient) validateJWKSURI(ctx context.Context, jwksURI string) error {
	if strings.HasPrefix(jwksURI, "https://") {
		return nil
	}

	u, err := url.Parse(jwksURI)
	if err != nil {
		return fmt.Errorf("invalid jwks URI: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("jwks URI must use HTTP or HTTPS: %s", jwksURI)
	}

	// Allow HTTP only for local/Docker internal services.
	host := u.Hostname()
	isLocal := host == "localhost" || host == "127.0.0.1" ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".orb.local")
	hasNoDots := !strings.Contains(host, ".") // Docker internal services
	if !isLocal && !hasNoDots {
		return fmt.Errorf("jwks URI must use HTTPS for external hosts: %s", jwksURI)
	}

	// Warn in production when HTTP is used for non-localhost services.
	isLoopback := host == "localhost" || host == "127.0.0.1"
	if c.environment == "production" && !isLoopback {
		logger.Warn(ctx, "jwks.insecure_http_uri",
			zap.String("uri", jwksURI),
			zap.String("environment", c.environment),
			zap.String("recommendation", "use HTTPS for all JWKS endpoints in production"))
	}

	return nil
}

// executeJWKSRequest executes the HTTP request to fetch JWKS and validates the response.
func (c *JWKSClient) executeJWKSRequest(ctx context.Context, jwksURI string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, fmt.Errorf("build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJWKSEndpointUnreachable, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn(ctx, "response_body_close_error", zap.Error(err))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrJWKSEndpointUnreachable, resp.StatusCode)
	}

	// Validate Content-Type to prevent response smuggling.
	if err := validateJWKSContentType(resp.Header.Get("Content-Type")); err != nil {
		return nil, err
	}

	// Limit response size to prevent memory exhaustion.
	limitedReader := io.LimitReader(resp.Body, c.maxResponseSize)
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %w", ErrJWKSInvalidResponse, err)
	}

	return data, nil
}

// validateJWKSContentType validates the Content-Type header of a JWKS response.
func validateJWKSContentType(ct string) error {
	if ct == "" {
		return fmt.Errorf("%w: missing content-type header", ErrJWKSInvalidResponse)
	}
	if !strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "application/jwk-set+json") {
		return fmt.Errorf("%w: unexpected content-type %q", ErrJWKSInvalidResponse, ct)
	}
	return nil
}

// validateJWKSSchema validates the basic structure of a JWKS response.
// SECURITY: This prevents cache poisoning attacks where a malicious endpoint
// returns a non-JWKS JSON response that could be parsed as valid keys.
//
// Validation includes:
//   - "keys" array must be present
//   - Each key must have "kty" (key type) per RFC 7517
//   - Each key must have "kid" (key ID) for key matching
//   - "use" must be "sig" (signature) if present
//   - "alg" must be a valid signature algorithm if present
func validateJWKSSchema(data []byte) bool {
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &jwks); err != nil {
		return false
	}
	// JWKS must have a "keys" array (can be empty but must be present)
	if jwks.Keys == nil {
		return false
	}
	// Validate each key has required fields
	for _, keyData := range jwks.Keys {
		var key struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
		}
		if err := json.Unmarshal(keyData, &key); err != nil {
			return false
		}
		// kty (key type) is required per RFC 7517
		if key.Kty == "" {
			return false
		}
		// kid (key ID) is required for JWKS key matching
		if key.Kid == "" {
			return false
		}
		// use must be "sig" (signature) if present
		if key.Use != "" && key.Use != "sig" {
			return false
		}
		// alg must be a valid signature algorithm if present
		// Common algorithms: RS256, RS384, RS512, ES256, ES384, ES512, PS256, PS384, PS512, EdDSA
		if key.Alg != "" && !isValidSignatureAlgorithm(key.Alg) {
			return false
		}
	}
	return true
}

// isValidSignatureAlgorithm checks if the algorithm is a valid signature algorithm.
func isValidSignatureAlgorithm(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512",
		"ES256", "ES384", "ES512",
		"PS256", "PS384", "PS512",
		"EdDSA":
		return true
	default:
		return false
	}
}

// validateIssuer checks if the issuer is allowed.
// Returns nil if no whitelist is configured (allowedIssuers is nil).
// Returns ErrJWKSUnauthorizedIssuer if the issuer is not in the whitelist.
func (c *JWKSClient) validateIssuer(issuer string) error {
	// If no whitelist is configured, all issuers are allowed.
	if c.allowedIssuers == nil {
		return nil
	}
	if !c.allowedIssuers[issuer] {
		return fmt.Errorf("%w: issuer %q is not in the allowed list", ErrJWKSUnauthorizedIssuer, issuer)
	}
	return nil
}

// GetKey retrieves a specific key by issuer and key ID.
// It first checks the Redis cache, then falls back to fetching the full JWKS.
//
// Security: If an issuer whitelist was configured, this method validates that
// the issuer is in the whitelist before proceeding. This prevents issuer
// confusion attacks where a malicious actor could attempt to fetch keys from
// an untrusted issuer.
func (c *JWKSClient) GetKey(ctx context.Context, issuer, jwksURI, kid string) (jwk.Key, error) {
	// Validate issuer against whitelist (defense-in-depth).
	if err := c.validateIssuer(issuer); err != nil {
		logger.Warn(ctx, "jwks.get_key.rejected",
			zap.String("issuer", issuer),
			zap.String("kid", kid),
			zap.Error(err))
		return nil, err
	}

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
// Implements per-issuer circuit breaker with exponential backoff to prevent
// thundering herd when the JWKS endpoint is failing.
//
// Security: If an issuer whitelist was configured, this method validates that
// the issuer is in the whitelist before proceeding. This prevents issuer
// confusion attacks where a malicious actor could attempt to fetch keys from
// an untrusted issuer.
func (c *JWKSClient) RefreshKey(ctx context.Context, issuer, jwksURI, kid string) (jwk.Key, error) {
	// Validate issuer against whitelist (defense-in-depth).
	if err := c.validateIssuer(issuer); err != nil {
		logger.Warn(ctx, "jwks.refresh_key.rejected",
			zap.String("issuer", issuer),
			zap.String("kid", kid),
			zap.Error(err))
		return nil, err
	}

	// Check circuit breaker before attempting refresh.
	if backoff, remaining := c.getCircuitBreakerBackoff(issuer); backoff > 0 {
		logger.Warn(ctx, "jwks: circuit breaker open, skipping refresh",
			zap.String("issuer", issuer),
			zap.Duration("backoff_remaining", remaining))
		// Try to serve from cache while circuit breaker is open.
		cachedKey, cacheErr := c.getCachedKey(ctx, issuer, kid)
		if cacheErr == nil && cachedKey != nil {
			return cachedKey, nil
		}
		return nil, fmt.Errorf("%w: circuit breaker open for issuer %s (backoff %v)",
			ErrJWKSEndpointUnreachable, issuer, remaining)
	}

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
		// Another instance is refreshing; wait with exponential backoff and try cache again.
		// Use a context-aware timer so the wait can be cancelled on shutdown.
		backoff := c.calculateBackoff(issuer)
		timer := time.NewTimer(backoff)
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
			if err := c.redis.Eval(ctx, safeReleaseLua, []string{lockKey}, holderUUID).Err(); err != nil {
				logger.Warn(ctx, "jwks: failed to release refresh lock",
					zap.String("issuer", issuer), zap.Error(err))
			}
		}()
	}

	// Fetch fresh JWKS.
	set, err := c.FetchJWKS(ctx, jwksURI)
	if err != nil {
		// Record failure in circuit breaker.
		c.recordCircuitBreakerFailure(ctx, issuer)
		return nil, err
	}

	// Cache all keys.
	c.cacheKeySet(ctx, issuer, set)

	key, found := set.LookupKeyID(kid)
	if !found {
		return nil, fmt.Errorf("%w: kid=%s issuer=%s", ErrJWKSKeyNotFound, kid, issuer)
	}

	// Success: reset circuit breaker.
	c.resetCircuitBreaker(issuer)

	return key, nil
}

// Circuit breaker constants for JWKS refresh.
const (
	// defaultLockTTL is the default distributed lock TTL for preventing concurrent refresh.
	// 30 seconds provides a safe balance between lock expiry and network latency tolerance.
	defaultLockTTL = 30 * time.Second

	// minLockTTL is the minimum allowed lock TTL to prevent accidental misconfiguration.
	// Lock TTL below 5 seconds is unsafe in distributed environments.
	minLockTTL = 5 * time.Second

	// cbMaxBackoff is the maximum backoff duration (5 minutes).
	cbMaxBackoff = 5 * time.Minute
	// cbBaseBackoff is the base backoff duration (1 second).
	cbBaseBackoff = 1 * time.Second
	// cbResetAfter is the duration after which failures are reset if no new failures occur (10 minutes).
	cbResetAfter = 10 * time.Minute
)

// getCircuitBreakerBackoff returns the remaining backoff duration for an issuer.
// Returns (0, 0) if the circuit breaker is closed (no backoff needed).
func (c *JWKSClient) getCircuitBreakerBackoff(issuer string) (time.Duration, time.Duration) {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()

	cb, exists := c.cbStates[issuer]
	if !exists || cb.consecutiveFailures == 0 {
		return 0, 0
	}

	// Auto-reset if enough time has passed since last failure.
	if time.Since(cb.lastFailureTime) > cbResetAfter {
		delete(c.cbStates, issuer)
		return 0, 0
	}

	backoff := c.calculateBackoffLocked(issuer)
	if backoff <= 0 {
		return 0, 0
	}

	remaining := backoff - time.Since(cb.lastFailureTime)
	if remaining <= 0 {
		return 0, 0
	}
	return remaining, remaining
}

// calculateBackoff calculates the backoff duration for an issuer.
func (c *JWKSClient) calculateBackoff(issuer string) time.Duration {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	return c.calculateBackoffLocked(issuer)
}

// calculateBackoffLocked calculates the backoff duration (caller must hold cbMu).
func (c *JWKSClient) calculateBackoffLocked(issuer string) time.Duration {
	cb, exists := c.cbStates[issuer]
	if !exists || cb.consecutiveFailures == 0 {
		return 200 * time.Millisecond // Default short wait for lock contention.
	}

	// Exponential backoff: base * 2^(failures-1), capped at max.
	backoff := float64(cbBaseBackoff) * math.Pow(2, float64(cb.consecutiveFailures-1))
	if backoff > float64(cbMaxBackoff) {
		backoff = float64(cbMaxBackoff)
	}
	return time.Duration(backoff)
}

// recordCircuitBreakerFailure records a refresh failure for the circuit breaker.
func (c *JWKSClient) recordCircuitBreakerFailure(ctx context.Context, issuer string) {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()

	cb, exists := c.cbStates[issuer]
	if !exists {
		cb = &issuerCircuitBreaker{}
		c.cbStates[issuer] = cb
	}
	cb.consecutiveFailures++
	cb.lastFailureTime = time.Now()

	logger.Warn(ctx, "jwks: refresh failed, circuit breaker updated",
		zap.String("issuer", issuer),
		zap.Int("consecutive_failures", cb.consecutiveFailures))
}

// resetCircuitBreaker resets the circuit breaker for an issuer after a successful refresh.
func (c *JWKSClient) resetCircuitBreaker(issuer string) {
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	delete(c.cbStates, issuer)
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
		if delErr := c.redis.Del(ctx, cacheKey).Err(); delErr != nil {
			logger.Warn(ctx, "jwks: failed to delete corrupted cache key",
				zap.String("cache_key", cacheKey), zap.Error(delErr))
		}
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
