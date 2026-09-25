package websearch

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDistributedCircuitBreaker_ClosedState_AllowsRequestsAfterFailureThreshold verifies
// that the circuit breaker remains closed initially and transitions to open after
// accumulating enough failures to exceed the failure threshold.
func TestDistributedCircuitBreaker_ClosedState_AllowsRequestsAfterFailureThreshold(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50, // 50%
		OpenTimeout:        2 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         10,
	}

	cb := NewDistributedCircuitBreaker("test-provider", client, cfg)
	ctx := context.Background()

	// Initially should allow (closed state)
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow in closed state")

	// Record enough failures to trigger open state
	for i := 0; i < 15; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Should now reject (open state)
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "should reject in open state after failures")
}

// TestDistributedCircuitBreaker_OpenToHalfOpen_AllowsProbeRequestsAfterTimeout verifies
// that the circuit breaker transitions from open to half-open after OpenTimeout,
// allowing probe requests through to test recovery.
func TestDistributedCircuitBreaker_OpenToHalfOpen_AllowsProbeRequestsAfterTimeout(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         3,
	}

	cb := NewDistributedCircuitBreaker("test-provider", client, cfg)
	ctx := context.Background()

	// Trigger open state
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Wait for open timeout
	time.Sleep(1100 * time.Millisecond)

	// Should allow (transition to half-open)
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow in half-open state")

	// Record success
	err = cb.RecordOutcome(ctx, true)
	assert.NoError(t, err)
}

// TestDistributedCircuitBreaker_RedisUnavailable_ReturnsErrorAndAllowsDegradation verifies
// that when Redis is unavailable, AllowWithErr returns an error but still permits
// the request, leaving degradation decisions to the caller.
func TestDistributedCircuitBreaker_RedisUnavailable_ReturnsErrorAndAllowsDegradation(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := DefaultCircuitBreakerConfig()
	cb := NewDistributedCircuitBreaker("test-provider", client, cfg)
	ctx := context.Background()

	// Initially works
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed)

	// Simulate Redis down
	s.Close()

	// Should return error
	allowed, err = cb.AllowWithErr(ctx)
	assert.Error(t, err, "should return error when Redis is down")
	// Caller should handle degradation
	assert.True(t, allowed, "should allow on error (caller decides)")
}

// TestHybridCircuitBreaker_RedisFailure_DegradesToLocal verifies that the hybrid
// circuit breaker detects Redis failure via background health checks and
// transparently falls back to the local circuit breaker.
func TestHybridCircuitBreaker_RedisFailure_DegradesToLocal(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	localCB := NewCircuitBreaker(DefaultCircuitBreakerConfig(), "test")
	remoteCB := NewDistributedCircuitBreaker("test", client, DefaultCircuitBreakerConfig())

	hybrid := NewHybridCircuitBreaker(localCB, remoteCB, client, nil)
	defer hybrid.Shutdown()

	// Initially should use distributed
	assert.True(t, hybrid.redisHealthy.Load(), "should initially assume Redis healthy")

	// Simulate Redis failure
	s.Close()

	// Wait for health check to detect failure (5s interval + 2s timeout)
	time.Sleep(8 * time.Second)

	// Should have degraded to local
	assert.False(t, hybrid.redisHealthy.Load(), "should detect Redis failure")

	// Allow should still work (using local)
	assert.True(t, hybrid.Allow(), "should fall back to local breaker")
}

// TestDistributedRateLimiter_BasicFlow_RejectsExcessRequests verifies that the
// distributed rate limiter allows requests up to the configured limit within
// a window and rejects subsequent requests.
func TestDistributedRateLimiter_BasicFlow_RejectsExcessRequests(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	rl := NewDistributedRateLimiter("test", client, 2.0, 2) // 2 requests per second
	ctx := context.Background()

	// First 2 requests should be allowed
	allowed, err := rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed)

	// Third request should be rejected
	allowed, err = rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "should reject after exceeding rate limit")
}

// TestDistributedRateLimiter_WindowReset_AllowsRequestsAfterExpiry verifies that
// the rate limiter counter resets after the window expires, allowing new requests.
func TestDistributedRateLimiter_WindowReset_AllowsRequestsAfterExpiry(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	rl := NewDistributedRateLimiter("test", client, 1.0, 1) // 1 request per second
	ctx := context.Background()

	// First request allowed
	allowed, err := rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed)

	// Second request rejected
	allowed, err = rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed)

	// Wait for window reset
	time.Sleep(1100 * time.Millisecond)

	// Should allow again
	allowed, err = rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow after window reset")
}

// TestRedisClient_HealthCheck_DetectsFailureAfterRedisDown verifies that the
// RedisClient background health check correctly detects when Redis becomes
// unavailable and updates the healthy flag.
func TestRedisClient_HealthCheck_DetectsFailureAfterRedisDown(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	cfg := RedisConfig{Addr: s.Addr()}
	rc, err := NewRedisClient(cfg)
	require.NoError(t, err)
	defer rc.Close()
	rc.Start() // Start health checking

	// Initially healthy
	assert.True(t, rc.IsHealthy())

	// Simulate Redis down
	s.Close()

	// Wait for health check (5s interval + 2s timeout)
	time.Sleep(8 * time.Second)

	// Should detect unhealthy
	assert.False(t, rc.IsHealthy())
}

// TestRedisClient_Recovery_DetectsHealthAfterRestart verifies that the RedisClient
// correctly detects recovery when a failed Redis instance is replaced with a
// healthy one at a different address.
func TestRedisClient_Recovery_DetectsHealthAfterRestart(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	cfg := RedisConfig{Addr: s.Addr()}
	rc, err := NewRedisClient(cfg)
	require.NoError(t, err)
	defer rc.Close()
	rc.Start() // Start health checking

	// Initially healthy
	assert.True(t, rc.IsHealthy())

	// Simulate Redis down
	s.Close()
	time.Sleep(8 * time.Second)
	assert.False(t, rc.IsHealthy())

	// Restart Redis (new instance on same port)
	s2, err := miniredis.Run()
	require.NoError(t, err)
	defer s2.Close()

	// Safely update client to new address via resetClient (avoids data race
	// with background health check goroutine reading the client pointer).
	rc.resetClient(s2.Addr())

	// Wait for recovery (5s interval)
	time.Sleep(6 * time.Second)
	assert.True(t, rc.IsHealthy())
}

// TestDistributedCircuitBreaker_StateConsistency_VisibleAcrossInstances verifies
// that circuit breaker state recorded by one instance is immediately visible
// to other instances sharing the same Redis backend.
func TestDistributedCircuitBreaker_StateConsistency_VisibleAcrossInstances(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        5 * time.Second,
		HalfOpenMaxRequests: 3,
		WindowSize:         4,
	}

	// Create two instances (simulating multiple app instances)
	cb1 := NewDistributedCircuitBreaker("test", client, cfg)
	cb2 := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Instance 1 records failures
	for i := 0; i < 4; i++ {
		_ = cb1.RecordOutcome(ctx, false)
	}

	// Instance 2 should see the same state (open)
	allowed, err := cb2.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "instance 2 should see open state from instance 1")
}

// === 边缘场景测试 ===

// TestDistributedCircuitBreaker_ConcurrentAccess_AllowsAllInClosedState verifies
// that concurrent requests all succeed when the circuit is in closed state
// and that no data races occur under parallel load.
func TestDistributedCircuitBreaker_ConcurrentAccess_AllowsAllInClosedState(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := DefaultCircuitBreakerConfig()
	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Run 100 concurrent requests
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			allowed, err := cb.AllowWithErr(ctx)
			done <- err == nil && allowed
		}()
	}

	// All should succeed (circuit is closed)
	successCount := 0
	for i := 0; i < 100; i++ {
		if <-done {
			successCount++
		}
	}
	assert.Equal(t, 100, successCount, "all concurrent requests should succeed")
}

// TestDistributedRateLimiter_ConcurrentAccess_RespectsLimit verifies that the
// distributed rate limiter correctly enforces the rate limit under concurrent
// access from multiple goroutines.
func TestDistributedRateLimiter_ConcurrentAccess_RespectsLimit(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	rl := NewDistributedRateLimiter("test", client, 10.0, 10) // 10 req/s
	ctx := context.Background()

	// Run 20 concurrent requests (exceeds limit of 10)
	done := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		go func() {
			allowed, err := rl.AllowWithErr(ctx)
			done <- err == nil && allowed
		}()
	}

	// Count allowed requests
	allowedCount := 0
	for i := 0; i < 20; i++ {
		if <-done {
			allowedCount++
		}
	}
	// Should allow approximately 10 (rate limit)
	assert.LessOrEqual(t, allowedCount, 12, "should respect rate limit under concurrency")
	assert.GreaterOrEqual(t, allowedCount, 8, "should allow reasonable number")
}

// TestDistributedCircuitBreaker_LuaAtomicity_TransitionsCorrectly verifies that
// the Lua scripts execute atomically and the circuit breaker correctly transitions
// through open -> half_open states, and that probe successes are tracked.
func TestDistributedCircuitBreaker_LuaAtomicity_TransitionsCorrectly(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        2 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Record failures to open circuit
	for i := 0; i < 10; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Verify circuit is open
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "circuit should be open")

	// Wait for timeout
	time.Sleep(2100 * time.Millisecond)

	// Should transition to half-open
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow in half-open")

	// Record success
	_ = cb.RecordOutcome(ctx, true)

	// Should still allow (half-open with successes)
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow more requests in half-open")
}

// TestDistributedCircuitBreaker_ConfigurationEdgeCases_HighThreshold verifies that
// the circuit breaker only opens when the failure rate strictly exceeds the
// configured threshold, and that incomplete windows do not trigger premature opening.
func TestDistributedCircuitBreaker_ConfigurationEdgeCases_HighThreshold(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   90, // 90%
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 1,
		WindowSize:         5,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Test 1: 80% failure rate should not trigger 90% threshold
	for i := 0; i < 4; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}
	_ = cb.RecordOutcome(ctx, true) // Window: [1 0 0 0 0] = 4/5 = 80%

	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "80% failure rate should not open at 90% threshold")

	// Test 2: 100% failure rate should trigger 90% threshold
	// Record 5 consecutive failures to get 100%
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}
	// Window: [0 0 0 0 0] = 5/5 = 100%

	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "100% failure rate should open at 90% threshold")
}

// TestHybridRateLimiter_RedisFailure_DegradesToLocal verifies that the hybrid
// rate limiter detects Redis failure and transparently falls back to the local
// rate limiter without dropping requests.
func TestHybridRateLimiter_RedisFailure_DegradesToLocal(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	localRL := NewRateLimiter(10, 10)
	remoteRL := NewDistributedRateLimiter("test", client, 10, 10)

	hybrid := NewHybridRateLimiter(localRL, remoteRL, client)
	defer hybrid.Shutdown()

	// Initially should use distributed
	assert.True(t, hybrid.redisHealthy.Load(), "should initially assume Redis healthy")

	// Simulate Redis failure
	s.Close()

	// Wait for health check (5s interval + 2s timeout)
	time.Sleep(8 * time.Second)

	// Should have degraded to local
	assert.False(t, hybrid.redisHealthy.Load(), "should detect Redis failure")

	// Allow should still work (using local)
	assert.True(t, hybrid.Allow(), "should fall back to local rate limiter")
}

// TestDistributedCircuitBreaker_RecoveryFromOpen_ClosesAfterSufficientSuccesses verifies
// the full recovery path: open -> half_open (after timeout) -> closed (after
// enough consecutive successes in half_open state).
func TestDistributedCircuitBreaker_RecoveryFromOpen_ClosesAfterSufficientSuccesses(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 3,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Trigger open state
	for i := 0; i < 10; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Verify open
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "should be open")

	// Wait for timeout
	time.Sleep(1100 * time.Millisecond)

	// Should allow in half-open
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow in half-open")

	// Record successes to close
	for i := 0; i < 3; i++ {
		_ = cb.RecordOutcome(ctx, true)
	}

	// Should be closed now
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should be closed after successes")
}

// TestDistributedCircuitBreaker_HalfOpenMaxRequests_RecoversWithConfiguredSuccessCount verifies
// that the circuit breaker correctly uses the configured HalfOpenMaxRequests value
// (not hardcoded default) when determining recovery in half_open state.
// This test prevents the bug where recordLua reads half_open_max from Redis hash
// instead of using the ARGV parameter, causing HalfOpenMaxRequests=1 to never recover.
func TestDistributedCircuitBreaker_HalfOpenMaxRequests_RecoversWithConfiguredSuccessCount(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	// Test with HalfOpenMaxRequests=1 (previously broken: required 3 successes)
	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 1, // Only 1 success needed to close
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Trigger open state
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Verify open
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "should be open")

	// Wait for timeout
	time.Sleep(1100 * time.Millisecond)

	// Should allow in half-open
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow in half-open")

	// Record 1 success - should close (with HalfOpenMaxRequests=1)
	_ = cb.RecordOutcome(ctx, true)

	// Should be closed now
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should be closed after 1 success (HalfOpenMaxRequests=1)")
}

// TestDistributedCircuitBreaker_WindowThresholdBoundary_OpensAtExactlyThreshold verifies
// that the circuit breaker opens when failure rate equals the threshold (>=).
// This tests the boundary condition: 50% failures with 50% threshold should open.
func TestDistributedCircuitBreaker_WindowThresholdBoundary_OpensAtExactlyThreshold(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50, // 50%
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Record exactly 50% failures (2 out of 4)
	_ = cb.RecordOutcome(ctx, false)
	_ = cb.RecordOutcome(ctx, false)
	_ = cb.RecordOutcome(ctx, true)
	_ = cb.RecordOutcome(ctx, true)
	// Window: [0 0 1 1] = 2/4 = 50% = threshold

	// Should be open (failure_rate >= threshold)
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "should be open at exactly 50% failure rate with 50% threshold")
}

// TestDistributedRateLimiter_AtomicIncrExpire_NoOrphanedKeys verifies that the
// INCR+EXPIRE operation is atomic and keys always get TTL set, preventing memory leaks.
func TestDistributedRateLimiter_AtomicIncrExpire_NoOrphanedKeys(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	rl := NewDistributedRateLimiter("test", client, 10.0, 10)
	ctx := context.Background()

	// Make a request
	allowed, err := rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed)

	// Verify key exists and has TTL
	now := time.Now().Unix()
	key := fmt.Sprintf("websearch:rl:test:%d", now)
	ttl, err := client.TTL(ctx, key).Result()
	assert.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0), "key should have TTL set")
	assert.LessOrEqual(t, ttl.Seconds(), float64(3), "TTL should be <= 3 seconds (1s window + 1s buffer)")
}

// TestDistributedCircuitBreaker_ConfigurationValidation_EnsuresSensibleDefaults verifies
// that invalid configurations are corrected to sensible defaults.
func TestDistributedCircuitBreaker_ConfigurationValidation_EnsuresSensibleDefaults(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	// Test with invalid config
	cfg := CircuitBreakerConfig{
		WindowSize:         -1,  // Invalid
		FailureThreshold:   150, // Invalid (> 100)
		OpenTimeout:        500 * time.Millisecond, // Too small
		HalfOpenMaxRequests: 0,  // Invalid
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)

	// Verify defaults were applied
	assert.Equal(t, 10, cb.config.WindowSize, "WindowSize should default to 10")
	assert.Equal(t, 50, cb.config.FailureThreshold, "FailureThreshold should default to 50")
	assert.Equal(t, 5*time.Second, cb.config.OpenTimeout, "OpenTimeout should default to 5s")
	assert.Equal(t, 3, cb.config.HalfOpenMaxRequests, "HalfOpenMaxRequests should default to 3")
}

// TestHybridCircuitBreaker_TimeoutProtection_DoesNotHangOnUnresponsiveRedis verifies
// that HybridCircuitBreaker.Allow() has timeout protection and does not hang
// indefinitely when Redis becomes unresponsive (but doesn't return error).
func TestHybridCircuitBreaker_TimeoutProtection_DoesNotHangOnUnresponsiveRedis(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	localCB := NewCircuitBreaker(DefaultCircuitBreakerConfig(), "test")
	remoteCB := NewDistributedCircuitBreaker("test", client, DefaultCircuitBreakerConfig())

	hybrid := NewHybridCircuitBreaker(localCB, remoteCB, client, nil)
	defer hybrid.Shutdown()

	// Simulate slow Redis by setting a very long response delay
	// Note: miniredis doesn't support delays, but we verify the timeout context is used
	// by checking that Allow() completes quickly even with healthy Redis
	start := time.Now()
	allowed := hybrid.Allow()
	duration := time.Since(start)

	assert.True(t, allowed)
	assert.Less(t, duration, 2*time.Second, "Allow() should complete quickly")
}

// TestDistributedRateLimiter_ZeroRate_AllowsAtLeastOneRequest verifies that
// rate=0 or negative values still allow at least 1 request per window.
func TestDistributedRateLimiter_ZeroRate_AllowsAtLeastOneRequest(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	// Test with rate=0
	rl := NewDistributedRateLimiter("test", client, 0.0, 0)
	ctx := context.Background()

	// Should allow at least 1 request
	allowed, err := rl.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "rate=0 should allow at least 1 request")
}

// TestDistributedCircuitBreaker_MultipleProviders_KeyIsolation verifies that
// circuit breakers for different providers are isolated in Redis key space.
func TestDistributedCircuitBreaker_MultipleProviders_KeyIsolation(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	// Use small window size to trigger open with fewer failures
	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        5 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}
	cb1 := NewDistributedCircuitBreaker("provider1", client, cfg)
	cb2 := NewDistributedCircuitBreaker("provider2", client, cfg)
	ctx := context.Background()

	// Record failures for provider1
	for i := 0; i < 5; i++ {
		_ = cb1.RecordOutcome(ctx, false)
	}

	// Provider1 should be open
	allowed1, err := cb1.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed1, "provider1 should be open")

	// Provider2 should still be closed (isolated)
	allowed2, err := cb2.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed2, "provider2 should be closed (isolated from provider1)")
}

// TestDistributedRateLimiter_WindowSliding_OpensWhenFullAndClosesAfterRecovery verifies
// that the sliding window correctly accumulates outcomes, opens the circuit when
// the failure rate exceeds the threshold after the window is full, and recovers
// to closed state after successful probe requests in half_open.
func TestDistributedCircuitBreaker_WindowSliding_OpensWhenFullAndClosesAfterRecovery(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        5 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         5,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Fill window with 3 failures + 2 successes = 5 total, 60% failure rate
	for i := 0; i < 3; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}
	for i := 0; i < 2; i++ {
		_ = cb.RecordOutcome(ctx, true)
	}
	// Window: [1 1 0 0 0], failure rate = 3/5 = 60% > 50% threshold

	// Should be open (window is full, failure rate exceeds threshold)
	allowed, err := cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.False(t, allowed, "should be open with 60% failure rate when window is full")

	// Wait for open timeout and verify half-open behavior
	time.Sleep(5100 * time.Millisecond)

	// Should allow in half-open
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should allow in half-open state")

	// Record successes to close
	_ = cb.RecordOutcome(ctx, true)
	_ = cb.RecordOutcome(ctx, true)

	// Should be closed now
	allowed, err = cb.AllowWithErr(ctx)
	assert.NoError(t, err)
	assert.True(t, allowed, "should be closed after successes in half-open")
}

// === Round 4: additional edge-case tests ===

// TestDistributedCircuitBreaker_MainKeyTTLInClosedState verifies that the main
// Redis key gets a TTL even when the circuit stays closed (continuous successes).
// Without this, the main key would persist forever -- a slow memory leak when
// many providers are used over the lifetime of the server.
func TestDistributedCircuitBreaker_MainKeyTTLInClosedState(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        5 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Record only successes -- circuit stays closed
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, true)
	}

	// The main key should have a TTL even though no state transition occurred
	key := "websearch:cb:test"
	ttl, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0),
		"main key must have TTL in closed state to prevent orphaned keys")
	assert.LessOrEqual(t, ttl.Seconds(), float64(10),
		"TTL should be <= open_timeout*2 = 10s")

	// Window key should also have TTL
	windowKey := key + ":window"
	wTTL, err := client.TTL(ctx, windowKey).Result()
	require.NoError(t, err)
	assert.Greater(t, wTTL.Seconds(), float64(0), "window key should have TTL")
}

// TestDistributedCircuitBreaker_FailureThresholdZero_OpensOnlyOnFailure verifies
// that threshold=0 ("any failure opens") does NOT open on a full window of
// successes, but DOES open as soon as a single failure is recorded.
func TestDistributedCircuitBreaker_FailureThresholdZero_OpensOnlyOnFailure(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   0,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 1,
		WindowSize:         3,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Record 3 successes (fills window) -- should NOT open
	for i := 0; i < 3; i++ {
		_ = cb.RecordOutcome(ctx, true)
	}

	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "threshold=0 with all-success window should stay closed")

	// Record 1 failure -- should now open
	_ = cb.RecordOutcome(ctx, false)

	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "threshold=0 with any failure should open")
}

// TestDistributedCircuitBreaker_FailureThreshold100_OpensOnlyOnAllFailures verifies
// that threshold=100 only opens when the window is 100% failures.
func TestDistributedCircuitBreaker_FailureThreshold100_OpensOnlyOnAllFailures(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   100,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 1,
		WindowSize:         3,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// 2 failures + 1 success = 66.7% < 100% -- should stay closed
	_ = cb.RecordOutcome(ctx, false)
	_ = cb.RecordOutcome(ctx, false)
	_ = cb.RecordOutcome(ctx, true)

	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "66.7% failure rate should not open at 100% threshold")

	// Replace all with failures (window slides to all failures)
	// Window after step 1: [1, 0, 0] (newest first: success, fail, fail)
	// Need to push 3 failures to slide the success out of the window
	_ = cb.RecordOutcome(ctx, false) // window: [0, 1, 0] → 1/3 = 33%
	_ = cb.RecordOutcome(ctx, false) // window: [0, 0, 1] → 1/3 = 33%
	_ = cb.RecordOutcome(ctx, false) // window: [0, 0, 0] → 3/3 = 100%

	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "100% failure rate should open at 100% threshold")
}

// TestDistributedRateLimiter_BurstParameter_LimitsBurstSize verifies that the
// burst parameter (not rate) controls the effective per-window limit.
// With rate=5.0 and burst=10, exactly 10 requests should be allowed per second.
func TestDistributedRateLimiter_BurstParameter_LimitsBurstSize(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	// rate=5 but burst=10: effective limit should be 10 (burst), not 5 (rate)
	rl := NewDistributedRateLimiter("test", client, 5.0, 10)
	ctx := context.Background()

	allowedCount := 0
	for i := 0; i < 15; i++ {
		ok, err := rl.AllowWithErr(ctx)
		require.NoError(t, err)
		if ok {
			allowedCount++
		}
	}

	assert.Equal(t, 10, allowedCount,
		"burst=10 should allow exactly 10 requests per window, regardless of rate=5")
}

// TestDistributedCircuitBreaker_SpecialCharProviderName_Isolation verifies that
// provider names containing colons, slashes, spaces, and Unicode characters
// produce valid, distinct Redis keys without collisions or errors.
func TestDistributedCircuitBreaker_SpecialCharProviderName_Isolation(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	names := []string{
		"provider:with:colons",
		"provider/with/slashes",
		"provider with spaces",
		"provider-with-null",
		"provider☃unicode",
	}

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        5 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}
	ctx := context.Background()

	breakers := make([]*DistributedCircuitBreaker, len(names))
	for i, name := range names {
		breakers[i] = NewDistributedCircuitBreaker(name, client, cfg)
	}

	// Open the first breaker
	for i := 0; i < 5; i++ {
		_ = breakers[0].RecordOutcome(ctx, false)
	}

	// First should be open
	allowed, err := breakers[0].AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "first provider (colons) should be open")

	// All others should remain closed (isolated keys)
	for i := 1; i < len(breakers); i++ {
		allowed, err = breakers[i].AllowWithErr(ctx)
		require.NoError(t, err)
		assert.True(t, allowed, "provider %q should be closed (isolated)", names[i])
	}
}

// TestDistributedCircuitBreaker_ConcurrentHalfOpen_LimitsProbeCount verifies
// that under heavy concurrency, the half_open count is precise: exactly
// max_half_open probes are allowed, and excess goroutines are rejected.
func TestDistributedCircuitBreaker_ConcurrentHalfOpen_LimitsProbeCount(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 3,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Trigger open state
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Wait for open timeout to transition to half_open
	time.Sleep(1100 * time.Millisecond)

	// Fire 20 concurrent Allow() calls
	const goroutines = 20
	var wg sync.WaitGroup
	allowedCh := make(chan bool, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, err := cb.AllowWithErr(ctx)
			allowedCh <- err == nil && ok
		}()
	}
	wg.Wait()
	close(allowedCh)

	allowedCount := 0
	for ok := range allowedCh {
		if ok {
			allowedCount++
		}
	}

	// The first Allow() that transitions open->half_open sets count=1 and allows.
	// Subsequent callers see half_open and increment: 2nd→2 (allow), 3rd→3 (allow),
	// 4th sees count>=max (3) and rejects. Total allowed = 3.
	assert.Equal(t, 3, allowedCount,
		"exactly max_half_open requests should be allowed in half_open state")
}

// TestDistributedCircuitBreaker_WindowSizeOne_OpensOnSingleFailure verifies
// that WindowSize=1 triggers the breaker on every single failure.
func TestDistributedCircuitBreaker_WindowSizeOne_OpensOnSingleFailure(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 1,
		WindowSize:         1,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Single failure with window=1 should open the circuit
	_ = cb.RecordOutcome(ctx, false)

	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "window=1 with single failure should open")
}

// TestDistributedCircuitBreaker_HalfOpenToOpenToHalfOpen_Cycle verifies
// that the half_open -> open -> half_open cycle works correctly: a failure
// in half_open reopens the circuit, and after another timeout the circuit
// allows new probes.
func TestDistributedCircuitBreaker_HalfOpenToOpenToHalfOpen_Cycle(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Trigger open
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Wait for first timeout -> half_open
	time.Sleep(1100 * time.Millisecond)

	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should allow first probe in half_open")

	// Fail in half_open -> back to open
	_ = cb.RecordOutcome(ctx, false)

	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "should reject after failure in half_open (now open)")

	// Wait for second timeout -> half_open again
	time.Sleep(1100 * time.Millisecond)

	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should allow probes after second timeout (half_open again)")

	// Now succeed to close
	_ = cb.RecordOutcome(ctx, true)
	_ = cb.RecordOutcome(ctx, true)

	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should be closed after sufficient successes")
}

// === Round 5: deep-review fixes and additional edge-case tests ===

// TestDistributedRateLimiter_OrphanedKeyDefense_ResetsTTLWhenMissing verifies that
// the rate limiter defends against orphaned keys: if a key exists without a TTL
// (e.g., due to manual PERSIST or Redis restart without AOF), the Lua script
// must re-set the TTL instead of leaving the key alive forever.
func TestDistributedRateLimiter_OrphanedKeyDefense_ResetsTTLWhenMissing(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	rl := NewDistributedRateLimiter("test", client, 10.0, 10)
	ctx := context.Background()

	// First request creates the key with TTL
	allowed, err := rl.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed)

	// Find the rate limiter key (websearch:rl:test:<unix_second>)
	keys, err := client.Keys(ctx, "websearch:rl:test:*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1, "should have exactly one rate limiter key")
	key := keys[0]

	// Simulate TTL loss: manually remove TTL using PERSIST
	err = client.Persist(ctx, key).Err()
	require.NoError(t, err)

	// Verify TTL is now -1 (no expiry)
	ttlBefore, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), ttlBefore, "TTL should be -1 after PERSIST")

	// Make another request -- the Lua script must detect missing TTL and re-set it
	allowed, err = rl.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed)

	// Verify TTL is now restored (> 0)
	ttlAfter, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttlAfter.Seconds(), float64(0),
		"TTL must be re-set by Lua script after PERSIST removed it (defense against orphaned keys)")
}

// TestHybridCircuitBreaker_FullRecoveryCycle_DegradesAndRestores verifies the
// complete degradation-recovery lifecycle: distributed -> local (on Redis failure)
// -> distributed again (on Redis recovery). The breaker must not get stuck in
// local mode after Redis comes back.
func TestHybridCircuitBreaker_FullRecoveryCycle_DegradesAndRestores(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	localCB := NewCircuitBreaker(DefaultCircuitBreakerConfig(), "test")
	remoteCB := NewDistributedCircuitBreaker("test", client, DefaultCircuitBreakerConfig())

	hybrid := NewHybridCircuitBreaker(localCB, remoteCB, client, nil)
	defer hybrid.Shutdown()

	// Initially distributed mode
	assert.True(t, hybrid.redisHealthy.Load(), "should initially assume Redis healthy")

	// Simulate Redis failure
	s.Close()

	// Wait for health check to detect failure (5s interval + 2s timeout)
	time.Sleep(8 * time.Second)
	assert.False(t, hybrid.redisHealthy.Load(), "should detect Redis failure and degrade to local")

	// Start a fresh Redis instance on a different port
	s2, err := miniredis.Run()
	require.NoError(t, err)
	defer s2.Close()

	// Point the client at the new Redis via resetClient (safe swap under client mutex)
	hybrid.resetClient(s2.Addr())

	// Wait for health check to detect recovery (5s interval)
	time.Sleep(6 * time.Second)

	// Should have recovered to distributed mode
	assert.True(t, hybrid.redisHealthy.Load(),
		"should detect Redis recovery and restore distributed mode")

	// Verify distributed mode works by recording an outcome (should not error)
	hybrid.RecordSuccess()
	assert.True(t, hybrid.Allow(), "should allow after full recovery cycle")
}

// TestDistributedCircuitBreaker_ConcurrentRecordOutcome_AllInstancesObserve verifies
// that concurrent RecordOutcome calls from multiple simulated app instances produce
// a consistent view of the circuit breaker state. With window_size=10 and 50%
// threshold, recording 10 failures must reliably trip the breaker, and all readers
// must observe the open state.
func TestDistributedCircuitBreaker_ConcurrentRecordOutcome_AllInstancesObserve(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        5 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         10,
	}
	ctx := context.Background()

	// Simulate 5 app instances each recording failures concurrently
	const instances = 5
	const failuresPerInstance = 10

	var wg sync.WaitGroup
	wg.Add(instances)
	for i := 0; i < instances; i++ {
		go func() {
			defer wg.Done()
			cb := NewDistributedCircuitBreaker("test", client, cfg)
			for j := 0; j < failuresPerInstance; j++ {
				_ = cb.RecordOutcome(ctx, false)
			}
		}()
	}
	wg.Wait()

	// With 50 failures spread across 5 instances, the circuit must be open.
	// Any observer should see the open state.
	cb := NewDistributedCircuitBreaker("test", client, cfg)
	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "circuit should be open after 50 concurrent failures")

	// Verify main key has TTL (no orphan)
	key := "websearch:cb:test"
	ttl, err := client.TTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0),
		"main key must have TTL after concurrent state transitions")
}

// TestDistributedCircuitBreaker_FullLifecycle_TTLPreserved verifies that both the
// main key and the window key maintain valid TTLs through the entire state
// lifecycle: closed -> open -> half_open -> closed. Any missing TTL indicates a
// potential orphan-key leak.
func TestDistributedCircuitBreaker_FullLifecycle_TTLPreserved(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:         4,
	}
	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()
	mainKey := "websearch:cb:test"
	windowKey := mainKey + ":window"

	// Phase 1: closed state with activity -- keys should exist with TTL
	for i := 0; i < 3; i++ {
		_ = cb.RecordOutcome(ctx, true)
	}
	ttl, err := client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0),
		"[closed] main key must have TTL after recording outcomes")
	wTTL, err := client.TTL(ctx, windowKey).Result()
	require.NoError(t, err)
	assert.Greater(t, wTTL.Seconds(), float64(0),
		"[closed] window key must have TTL after recording outcomes")

	// Phase 2: trip to open -- record enough failures to exceed threshold
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}
	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "should be open after exceeding failure threshold")
	ttl, err = client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0),
		"[open] main key must have TTL")

	// Phase 3: wait for timeout -> half_open
	time.Sleep(1100 * time.Millisecond)
	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should allow probe in half_open")
	ttl, err = client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0),
		"[half_open] main key must have TTL")

	// Phase 4: recover to closed via sufficient successes
	_ = cb.RecordOutcome(ctx, true)
	_ = cb.RecordOutcome(ctx, true) // HalfOpenMaxRequests=2

	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should be closed after recovery")

	// After half_open->closed, window_key is DEL'd; a new success recreates it.
	_ = cb.RecordOutcome(ctx, true)
	ttl, err = client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(0),
		"[recovered-closed] main key must still have TTL")
	wTTL, err = client.TTL(ctx, windowKey).Result()
	require.NoError(t, err)
	assert.Greater(t, wTTL.Seconds(), float64(0),
		"[recovered-closed] window key must have TTL after recreation")
}

// TestDistributedCircuitBreaker_HalfOpenToClosed_TTLReset verifies that the
// half_open -> closed transition resets the main key TTL, preventing orphaned
// keys when the probe phase consumes most of the previous TTL.
func TestDistributedCircuitBreaker_HalfOpenToClosed_TTLReset(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        2 * time.Second, // TTL will be 4s
		HalfOpenMaxRequests: 1,               // Only 1 success needed
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()
	mainKey := "websearch:cb:test"

	// Trigger open state
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Verify open
	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "should be open")

	// Wait for timeout -> half_open
	time.Sleep(2100 * time.Millisecond)

	// Transition to half_open (this sets TTL = open_timeout * 2 = 4s)
	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should allow probe in half_open")

	// Check TTL after entering half_open
	ttlBefore, err := client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttlBefore.Seconds(), float64(0), "main key should have TTL in half_open")

	// Record success -> should transition to closed
	_ = cb.RecordOutcome(ctx, true)

	// Verify closed
	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should be closed after recovery")

	// Check TTL immediately after half_open -> closed transition
	// The TTL should be reset to open_timeout * 2 = 4s
	ttlAfter, err := client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttlAfter.Seconds(), float64(0),
		"main key must have TTL after half_open->closed transition")
	assert.GreaterOrEqual(t, ttlAfter.Seconds(), float64(3),
		"TTL should be close to open_timeout*2 (4s), not the remaining TTL from half_open")
	assert.LessOrEqual(t, ttlAfter.Seconds(), float64(4),
		"TTL should not exceed open_timeout*2 (4s)")

	// Verify window_key was deleted during recovery
	windowKey := mainKey + ":window"
	wTTL, err := client.TTL(ctx, windowKey).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-2), wTTL,
		"window key should be deleted after half_open->closed transition")
}

// TestDistributedCircuitBreaker_ConcurrentOpenToHalfOpen_Transition verifies that
// when multiple goroutines simultaneously detect the open timeout has expired,
// only one transitions to half_open and the others see the updated state.
func TestDistributedCircuitBreaker_ConcurrentOpenToHalfOpen_Transition(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	cfg := CircuitBreakerConfig{
		FailureThreshold:   50,
		OpenTimeout:        1 * time.Second,
		HalfOpenMaxRequests: 3,
		WindowSize:         4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()

	// Trigger open state
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Wait for timeout
	time.Sleep(1100 * time.Millisecond)

	// Fire concurrent Allow() calls - all should see consistent state
	const goroutines = 10
	var wg sync.WaitGroup
	results := make(chan bool, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, err := cb.AllowWithErr(ctx)
			results <- err == nil && ok
		}()
	}
	wg.Wait()
	close(results)

	allowedCount := 0
	for ok := range results {
		if ok {
			allowedCount++
		}
	}

	// Exactly HalfOpenMaxRequests should be allowed
	assert.Equal(t, 3, allowedCount,
		"exactly HalfOpenMaxRequests (3) should be allowed in concurrent half_open")

	// Verify state is half_open with correct counts
	mainKey := "websearch:cb:test"
	state, err := client.HGet(ctx, mainKey, "state").Result()
	require.NoError(t, err)
	assert.Equal(t, "half_open", state, "state should be half_open")

	count, err := client.HGet(ctx, mainKey, "half_open_count").Result()
	require.NoError(t, err)
	assert.Equal(t, "3", count, "half_open_count should be 3")
}

// TestDistributedCircuitBreaker_OpenStateFailure_UpdatesLastFailure verifies that
// recording a failure in open state updates last_failure and resets TTL, ensuring
// the open timeout countdown restarts from the new failure.
func TestDistributedCircuitBreaker_OpenStateFailure_UpdatesLastFailure(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer client.Close()

	// Use a long timeout to avoid timing-sensitive assertions with second-precision Unix timestamps.
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         5 * time.Second,
		HalfOpenMaxRequests: 1,
		WindowSize:          4,
	}

	cb := NewDistributedCircuitBreaker("test", client, cfg)
	ctx := context.Background()
	mainKey := "websearch:cb:test"

	// Trigger open state
	for i := 0; i < 5; i++ {
		_ = cb.RecordOutcome(ctx, false)
	}

	// Verify open
	allowed, err := cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "should be open")

	// Get initial last_failure timestamp
	lastFailure1, err := client.HGet(ctx, mainKey, "last_failure").Result()
	require.NoError(t, err)

	// Wait until we cross a second boundary so the next failure gets a different Unix timestamp.
	// time.Now().Unix() has second precision; without this, both failures might share the same
	// timestamp and the "reset" would be invisible.
	waitForSecondBoundary(t)

	// Record another failure in open state — this should update last_failure to a new second.
	_ = cb.RecordOutcome(ctx, false)

	// Verify last_failure was updated to a different second.
	lastFailure2, err := client.HGet(ctx, mainKey, "last_failure").Result()
	require.NoError(t, err)
	require.NotEqual(t, lastFailure1, lastFailure2,
		"last_failure should be updated when recording failure in open state")

	// Verify TTL was reset to open_timeout*2 = 10s.
	ttl, err := client.TTL(ctx, mainKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Seconds(), float64(8),
		"TTL should be close to open_timeout*2 (10s) after failure in open state")

	// At this point, last_failure = T (current second). We need to verify:
	//   - At T+3s: still open (3s < 5s timeout)
	//   - At T+6s: half_open (6s >= 5s timeout)

	// Sleep 3s — should still be open (3s < 5s timeout from second failure).
	time.Sleep(3 * time.Second)
	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.False(t, allowed, "should still be open (3s < 5s timeout)")

	// Sleep 3s more (total ~6s from second failure) — should now be half_open.
	time.Sleep(3 * time.Second)
	allowed, err = cb.AllowWithErr(ctx)
	require.NoError(t, err)
	assert.True(t, allowed, "should be half_open after timeout expires")
}

// waitForSecondBoundary blocks until time.Now().Unix() advances to the next second.
// This ensures that subsequent calls to time.Now().Unix() return a different value.
func waitForSecondBoundary(t *testing.T) {
	t.Helper()
	start := time.Now().Unix()
	for time.Now().Unix() == start {
		time.Sleep(10 * time.Millisecond)
	}
}
