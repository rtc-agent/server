package websearch

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// HybridCircuitBreaker combines local and distributed circuit breakers
// with automatic degradation when Redis is unavailable
type HybridCircuitBreaker struct {
	local          *CircuitBreaker
	remote         *DistributedCircuitBreaker
	redisClient    *redis.Client
	redisHealthy   atomic.Bool
	redisCheckStop chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	internalCtx    context.Context
	internalCancel context.CancelFunc
	logger         *zap.Logger
}

// NewHybridCircuitBreaker creates a hybrid circuit breaker
func NewHybridCircuitBreaker(
	local *CircuitBreaker,
	remote *DistributedCircuitBreaker,
	redisClient *redis.Client,
	logger *zap.Logger,
) *HybridCircuitBreaker {
	if logger == nil {
		logger = zap.NewNop()
	}

	internalCtx, internalCancel := context.WithCancel(context.Background())
	h := &HybridCircuitBreaker{
		local:          local,
		remote:         remote,
		redisClient:    redisClient,
		redisCheckStop: make(chan struct{}),
		internalCtx:    internalCtx,
		internalCancel: internalCancel,
		logger:         logger,
	}

	// Initially assume Redis is available, start background health check
	h.redisHealthy.Store(true)
	h.wg.Add(1)
	go h.redisHealthLoop()

	return h
}

// Allow checks if a request is allowed
// Uses distributed breaker if Redis is healthy, otherwise falls back to local
func (h *HybridCircuitBreaker) Allow() bool {
	if h.redisHealthy.Load() && h.remote != nil {
		// Use timeout context to prevent hanging on unresponsive Redis
		ctx, cancel := context.WithTimeout(h.internalCtx, 2*time.Second)
		defer cancel()

		allowed, err := h.remote.AllowWithErr(ctx)
		if err != nil {
			// Redis communication failed, switch to local mode
			h.logger.Warn("Redis circuit breaker failed, falling back to local",
				zap.Error(err))
			h.redisHealthy.Store(false)
			return h.local.Allow()
		}
		return allowed
	}
	// Redis unhealthy, use local circuit breaker
	return h.local.Allow()
}

// RecordSuccess records a successful operation
// Updates both local and remote to keep them in sync
func (h *HybridCircuitBreaker) RecordSuccess() {
	// Always update local to keep it in sync
	h.local.RecordSuccess()

	// Also update remote if Redis is healthy
	if h.redisHealthy.Load() && h.remote != nil {
		// Use timeout context to prevent hanging on unresponsive Redis
		ctx, cancel := context.WithTimeout(h.internalCtx, 2*time.Second)
		defer cancel()

		if err := h.remote.RecordOutcome(ctx, true); err != nil {
			h.logger.Warn("Redis record success failed, falling back to local",
				zap.Error(err))
			h.redisHealthy.Store(false)
		}
	}
}

// RecordFailure records a failed operation
// Updates both local and remote to keep them in sync
func (h *HybridCircuitBreaker) RecordFailure() {
	// Always update local to keep it in sync
	h.local.RecordFailure()

	// Also update remote if Redis is healthy
	if h.redisHealthy.Load() && h.remote != nil {
		// Use timeout context to prevent hanging on unresponsive Redis
		ctx, cancel := context.WithTimeout(h.internalCtx, 2*time.Second)
		defer cancel()

		if err := h.remote.RecordOutcome(ctx, false); err != nil {
			h.logger.Warn("Redis record failure failed, falling back to local",
				zap.Error(err))
			h.redisHealthy.Store(false)
		}
	}
}

// IsOpen returns whether the local circuit is open (read-only, no side effects).
// Note: This only checks the local circuit breaker state. When Redis is healthy,
// the distributed breaker is authoritative. If you need the effective state,
// use Allow() which consults the appropriate backend based on Redis health.
func (h *HybridCircuitBreaker) IsOpen() bool {
	return CircuitState(h.local.state.Load()) == StateOpen
}

// redisHealthLoop periodically checks Redis health
func (h *HybridCircuitBreaker) redisHealthLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.checkRedisHealth()
		case <-h.redisCheckStop:
			return
		case <-h.internalCtx.Done():
			return
		}
	}
}

// checkRedisHealth pings Redis and updates health status
func (h *HybridCircuitBreaker) checkRedisHealth() {
	ctx, cancel := context.WithTimeout(h.internalCtx, 2*time.Second)
	defer cancel()

	err := h.redisClient.Ping(ctx).Err()
	if err == nil {
		if !h.redisHealthy.Load() {
			h.logger.Info("Redis recovered, switching back to distributed mode")
			h.redisHealthy.Store(true)
		}
	} else {
		if h.redisHealthy.Load() {
			h.logger.Warn("Redis became unhealthy, switching to local mode",
				zap.Error(err))
			h.redisHealthy.Store(false)
		}
	}
}

// Shutdown stops the hybrid circuit breaker
func (h *HybridCircuitBreaker) Shutdown() {
	h.stopOnce.Do(func() {
		close(h.redisCheckStop)
		if h.internalCancel != nil {
			h.internalCancel()
		}
		h.wg.Wait()
	})
}
