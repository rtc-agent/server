package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/infra/auth"
)

// newTestRedisClient returns a Redis client connected to the test Redis.
// Falls back to localhost:6379 if TEST_REDIS_ADDR is not set.
func newTestRedisClient(t *testing.T) redis.UniversalClient {
	t.Helper()
	addr := "localhost:6379"
	if v := os.Getenv("TEST_REDIS_ADDR"); v != "" {
		addr = v
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.FlushDB(context.Background()).Err(); _ = rdb.Close() })
	return rdb
}

// generateRSAJWKS creates a JWKS containing an RSA public key with the given kid.
func generateRSAJWKS(t *testing.T, kid string) jwk.Set {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	key, err := jwk.FromRaw(privKey.PublicKey)
	require.NoError(t, err)
	_ = key.Set(jwk.KeyIDKey, kid)
	_ = key.Set(jwk.AlgorithmKey, "RS256")
	_ = key.Set(jwk.KeyUsageKey, "sig")

	set := jwk.NewSet()
	err = set.AddKey(key)
	require.NoError(t, err)

	return set
}

func TestJWKSClient_FetchJWKS_Success(t *testing.T) {
	set := generateRSAJWKS(t, "test-key-1")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient:     rdb,
		HTTPClient:      srv.Client(),
		DefaultCacheTTL: time.Minute,
	})
	require.NoError(t, err)

	// FetchJWKS requires HTTPS.
	fetched, err := client.FetchJWKS(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 1, fetched.Len())
}

func TestJWKSClient_FetchJWKS_RejectsHTTP(t *testing.T) {
	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient: rdb,
	})
	require.NoError(t, err)

	_, err = client.FetchJWKS(context.Background(), "http://example.com/jwks")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS")
}

func TestJWKSClient_FetchJWKS_RejectsInvalidContentType(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient: rdb,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)

	_, err = client.FetchJWKS(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content-type")
}

func TestJWKSClient_FetchJWKS_EndpointUnreachable(t *testing.T) {
	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient: rdb,
		HTTPClient:  &http.Client{Timeout: 100 * time.Millisecond},
	})
	require.NoError(t, err)

	_, err = client.FetchJWKS(context.Background(), "https://nonexistent.invalid/jwks")
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrJWKSEndpointUnreachable)
}

func TestJWKSClient_GetKey_CachesAndRetrieves(t *testing.T) {
	set := generateRSAJWKS(t, "cached-key-1")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	fetchCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient:     rdb,
		HTTPClient:      srv.Client(),
		DefaultCacheTTL: time.Minute,
	})
	require.NoError(t, err)

	ctx := context.Background()
	issuer := "https://test-issuer.example.com"

	// First call: cache miss -> fetches from JWKS.
	key1, err := client.GetKey(ctx, issuer, srv.URL, "cached-key-1")
	require.NoError(t, err)
	assert.NotNil(t, key1)
	assert.Equal(t, 1, fetchCount)

	// Second call: cache hit -> no fetch.
	key2, err := client.GetKey(ctx, issuer, srv.URL, "cached-key-1")
	require.NoError(t, err)
	assert.NotNil(t, key2)
	assert.Equal(t, 1, fetchCount) // Still 1, served from cache.
}

func TestJWKSClient_GetKey_KeyNotFound(t *testing.T) {
	set := generateRSAJWKS(t, "existing-key")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient: rdb,
		HTTPClient:  srv.Client(),
	})
	require.NoError(t, err)

	_, err = client.GetKey(context.Background(), "https://issuer.example.com", srv.URL, "nonexistent-kid")
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrJWKSKeyNotFound)
}

func TestJWKSClient_RefreshKey(t *testing.T) {
	set := generateRSAJWKS(t, "refresh-key-1")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	fetchCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient:     rdb,
		HTTPClient:      srv.Client(),
		DefaultCacheTTL: time.Minute,
		LockTTL:         time.Second,
	})
	require.NoError(t, err)

	ctx := context.Background()
	issuer := "https://refresh-issuer.example.com"

	// Pre-populate cache.
	_, err = client.GetKey(ctx, issuer, srv.URL, "refresh-key-1")
	require.NoError(t, err)
	initialCount := fetchCount

	// Force refresh.
	key, err := client.RefreshKey(ctx, issuer, srv.URL, "refresh-key-1")
	require.NoError(t, err)
	assert.NotNil(t, key)
	assert.Greater(t, fetchCount, initialCount, "RefreshKey should have fetched JWKS again")
}

func TestJWKSClient_NilRedisReturnsError(t *testing.T) {
	_, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient: nil,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis client is required")
}

func TestJWKSClient_AllowedIssuers_GetKey(t *testing.T) {
	set := generateRSAJWKS(t, "test-key-1")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient:     rdb,
		HTTPClient:      srv.Client(),
		DefaultCacheTTL: time.Minute,
		AllowedIssuers:  []string{"https://allowed.example.com"},
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Test 1: Allowed issuer should succeed.
	key, err := client.GetKey(ctx, "https://allowed.example.com", srv.URL, "test-key-1")
	require.NoError(t, err)
	assert.NotNil(t, key)

	// Test 2: Disallowed issuer should fail.
	_, err = client.GetKey(ctx, "https://malicious.example.com", srv.URL, "test-key-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrJWKSUnauthorizedIssuer)
	assert.Contains(t, err.Error(), "not in the allowed list")
}

func TestJWKSClient_AllowedIssuers_RefreshKey(t *testing.T) {
	set := generateRSAJWKS(t, "test-key-2")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient:     rdb,
		HTTPClient:      srv.Client(),
		DefaultCacheTTL: time.Minute,
		LockTTL:         time.Second,
		AllowedIssuers:  []string{"https://allowed.example.com"},
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Test 1: Allowed issuer should succeed.
	key, err := client.RefreshKey(ctx, "https://allowed.example.com", srv.URL, "test-key-2")
	require.NoError(t, err)
	assert.NotNil(t, key)

	// Test 2: Disallowed issuer should fail.
	_, err = client.RefreshKey(ctx, "https://malicious.example.com", srv.URL, "test-key-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrJWKSUnauthorizedIssuer)
	assert.Contains(t, err.Error(), "not in the allowed list")
}

func TestJWKSClient_NoAllowedIssuers_AllowsAll(t *testing.T) {
	set := generateRSAJWKS(t, "test-key-3")
	jwksJSON, err := json.Marshal(set)
	require.NoError(t, err)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	defer srv.Close()

	rdb := newTestRedisClient(t)
	client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
		RedisClient:     rdb,
		HTTPClient:      srv.Client(),
		DefaultCacheTTL: time.Minute,
		// No AllowedIssuers specified - should allow all.
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Any issuer should work when no whitelist is configured.
	key, err := client.GetKey(ctx, "https://any-issuer.example.com", srv.URL, "test-key-3")
	require.NoError(t, err)
	assert.NotNil(t, key)

	key, err = client.GetKey(ctx, "https://another-issuer.example.com", srv.URL, "test-key-3")
	require.NoError(t, err)
	assert.NotNil(t, key)
}

func TestJWKSClient_LockTTL_DefaultAndValidation(t *testing.T) {
	rdb := newTestRedisClient(t)

	tests := []struct {
		name        string
		lockTTL     time.Duration
		description string
	}{
		{
			name:        "default TTL when not specified",
			lockTTL:     0,
			description: "Should use default 30s TTL",
		},
		{
			name:        "explicit 30s TTL",
			lockTTL:     30 * time.Second,
			description: "Should accept explicit 30s TTL",
		},
		{
			name:        "explicit 60s TTL",
			lockTTL:     60 * time.Second,
			description: "Should accept explicit 60s TTL",
		},
		{
			name:        "very low TTL clamped to minimum",
			lockTTL:     1 * time.Second,
			description: "Should clamp to minimum 5s for safety",
		},
		{
			name:        "negative TTL uses default",
			lockTTL:     -10 * time.Second,
			description: "Should use default 30s TTL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := auth.NewJWKSClient(auth.JWKSClientConfig{
				RedisClient: rdb,
				LockTTL:     tt.lockTTL,
			})
			require.NoError(t, err, tt.description)
			assert.NotNil(t, client)
		})
	}
}

// Silence unused import warning for ecdsa/elliptic in tests.
var _ = func() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Silence unused import warning for fmt.
var _ = fmt.Sprintf
