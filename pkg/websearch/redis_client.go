package websearch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig defines Redis connection configuration
type RedisConfig struct {
	Addr     string `mapstructure:"addr"` // e.g., localhost:6379
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// DefaultRedisConfig returns sensible defaults
func DefaultRedisConfig() RedisConfig {
	return RedisConfig{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	}
}

// RedisClient wraps redis.Client with health checking
type RedisClient struct {
	client         *redis.Client
	config         RedisConfig
	healthy        bool
	healthMu       sync.RWMutex
	checkStop      chan struct{}
	stopOnce       sync.Once
	startOnce      sync.Once // Prevents multiple Start() calls
	wg             sync.WaitGroup
	internalCtx    context.Context
	internalCancel context.CancelFunc
}

// NewRedisClient creates a new Redis client
func NewRedisClient(config RedisConfig) (*RedisClient, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     config.Addr,
		Password: config.Password,
		DB:       config.DB,
	})

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connection failed: %w", err)
	}

	rc := &RedisClient{
		client:    client,
		config:    config,
		healthy:   true,
		checkStop: make(chan struct{}),
	}
	rc.internalCtx, rc.internalCancel = context.WithCancel(context.Background())

	return rc, nil
}

// Client returns the underlying redis.Client
// Thread-safe: acquires read lock to prevent races with resetClient
func (r *RedisClient) Client() *redis.Client {
	r.healthMu.RLock()
	defer r.healthMu.RUnlock()
	return r.client
}

// IsHealthy returns whether Redis is healthy
func (r *RedisClient) IsHealthy() bool {
	r.healthMu.RLock()
	defer r.healthMu.RUnlock()
	return r.healthy
}

// SetHealthy sets the health status
func (r *RedisClient) SetHealthy(healthy bool) {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	r.healthy = healthy
}

// Start begins background health checking
// Idempotent: multiple calls have no effect
func (r *RedisClient) Start() {
	r.startOnce.Do(func() {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					r.checkHealth()
				case <-r.checkStop:
					return
				case <-r.internalCtx.Done():
					return
				}
			}
		}()
	})
}

// Stop stops background health checking and closes the client
func (r *RedisClient) Stop() {
	r.stopOnce.Do(func() {
		close(r.checkStop)
		if r.internalCancel != nil {
			r.internalCancel()
		}
		r.wg.Wait()
		// Close Redis client here
		r.client.Close()
	})
}

// checkHealth performs a health check
func (r *RedisClient) checkHealth() {
	// Snapshot the client pointer under the health lock to prevent races
	// with resetClient (used in recovery/failover scenarios).
	r.healthMu.RLock()
	client := r.client
	r.healthMu.RUnlock()

	if client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.internalCtx, 2*time.Second)
	defer cancel()

	err := client.Ping(ctx).Err()
	if err == nil {
		if !r.IsHealthy() {
			// Redis recovered
			r.SetHealthy(true)
		}
	} else {
		if r.IsHealthy() {
			// Redis became unhealthy
			r.SetHealthy(false)
		}
	}
}

// resetClient safely replaces the underlying Redis client.
// This method acquires the health mutex to prevent races with the background
// checkHealth goroutine. Useful for testing recovery scenarios where the Redis
// address changes (e.g., failover to a different port).
func (r *RedisClient) resetClient(addr string) {
	r.healthMu.Lock()
	old := r.client
	r.client = redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: r.config.Password,
		DB:       r.config.DB,
	})
	r.healthMu.Unlock()
	if old != nil {
		_ = old.Close() // Best-effort close of the previous connection pool.
	}
}

// Close closes the Redis client (alias for Stop)
func (r *RedisClient) Close() error {
	r.Stop()
	return nil // Stop() already closed the client
}
