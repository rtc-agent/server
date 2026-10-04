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

// Silence unused import warning for ecdsa/elliptic in tests.
var _ = func() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Silence unused import warning for fmt.
var _ = fmt.Sprintf
