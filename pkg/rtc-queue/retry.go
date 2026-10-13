// Package rtcqueue provides a Redis-backed distributed queue.
package rtcqueue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// RetryConfig configures the retry behavior for Redis operations.
type RetryConfig struct {
	// MaxAttempts is the maximum number of attempts (including the first).
	// Must be >= 1. Default: 3.
	MaxAttempts int

	// InitialBackoff is the delay before the first retry.
	// Default: 100ms.
	InitialBackoff time.Duration

	// BackoffMultiplier is the factor by which the backoff increases.
	// Default: 2.0 (exponential backoff).
	BackoffMultiplier float64

	// MaxBackoff caps the backoff duration.
	// Default: 1s.
	MaxBackoff time.Duration

	// Logger is an optional logger for retry events.
	// If nil, the standard library log package is used.
	Logger RetryLogger
}

// RetryLogger is a minimal logging interface for retry events.
type RetryLogger interface {
	// Warn logs a warning message with key-value pairs.
	Warn(msg string, keysAndValues ...any)
}

// defaultRetryLogger wraps the standard library log package.
type defaultRetryLogger struct{}

func (l *defaultRetryLogger) Warn(msg string, keysAndValues ...any) {
	log.Printf("WARN: %s %v", msg, keysAndValues)
}

// DefaultRetryConfig returns a RetryConfig with sensible defaults:
// 3 attempts, exponential backoff starting at 100ms, capped at 1s.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:       3,
		InitialBackoff:    100 * time.Millisecond,
		BackoffMultiplier: 2.0,
		MaxBackoff:        1 * time.Second,
	}
}

// isRetryableRedisError returns true if the error is a transient network
// error that is safe to retry. Business errors (e.g., redis.Nil, Lua script
// errors) are NOT retryable.
//
// Retryable errors include:
//   - Connection refused / reset
//   - Timeout errors
//   - EOF (connection closed unexpectedly)
//   - Redis cluster failover indicators (MOVED, ASK during migration)
//
// Non-retryable errors include:
//   - redis.Nil (key not found - expected behavior)
//   - Lua script errors (business logic errors)
//   - Context cancelled/deadline exceeded (caller should control)
func isRetryableRedisError(err error) bool {
	if err == nil {
		return false
	}

	// Never retry context errors - caller controls the deadline
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Never retry redis.Nil - it's an expected "not found" result
	if errors.Is(err, redis.Nil) {
		return false
	}

	// Check for common transient network errors
	var redisErr redis.Error
	if errors.As(err, &redisErr) {
		// Redis errors that indicate transient issues
		msg := redisErr.Error()
		// Connection errors
		if contains(msg, "connection refused") ||
			contains(msg, "connection reset") ||
			contains(msg, "broken pipe") ||
			contains(msg, "i/o timeout") ||
			contains(msg, "EOF") ||
			contains(msg, "no such host") {
			return true
		}
		// Cluster redirection errors during failover
		if contains(msg, "MOVED") || contains(msg, "ASK") {
			return true
		}
		// Redis is loading
		if contains(msg, "LOADING") {
			return true
		}
		// Cluster down temporarily
		if contains(msg, "CLUSTERDOWN") {
			return true
		}
		// Try again (Redis busy)
		if contains(msg, "TRYAGAIN") {
			return true
		}
		// Busy - script taking too long, safe to retry
		if contains(msg, "BUSY") {
			return true
		}
		// Master down during failover
		if contains(msg, "READONLY") {
			return true
		}
	}

	// Also check for generic network errors wrapped without redis.Error
	errMsg := err.Error()
	if contains(errMsg, "connection refused") ||
		contains(errMsg, "connection reset") ||
		contains(errMsg, "broken pipe") ||
		contains(errMsg, "i/o timeout") ||
		contains(errMsg, "EOF") {
		return true
	}

	return false
}

// contains is a simple string contains check to avoid importing strings
// for this small utility.
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// RetryResult holds the outcome of a retry operation.
type RetryResult[T any] struct {
	Value      T
	Attempts   int
	TotalDelay time.Duration
}

// WithRetry executes the given operation with retry logic.
//
// The operation is retried only if:
//  1. It returns an error
//  2. The error is a retryable transient error (see isRetryableRedisError)
//  3. The maximum number of attempts has not been exceeded
//
// The operation receives a context that is cancelled when the overall
// retry budget is exhausted or when the parent context is cancelled.
//
// Example:
//
//	result, err := WithRetry(ctx, DefaultRetryConfig(), func(ctx context.Context) (string, error) {
//	    return rdb.Get(ctx, "key").Result()
//	})
func WithRetry[T any](ctx context.Context, cfg RetryConfig, op func(ctx context.Context) (T, error)) (RetryResult[T], error) {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 100 * time.Millisecond
	}
	if cfg.BackoffMultiplier < 1 {
		cfg.BackoffMultiplier = 2.0
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 1 * time.Second
	}

	log := cfg.Logger
	if log == nil {
		log = &defaultRetryLogger{}
	}

	var result RetryResult[T]
	backoff := cfg.InitialBackoff

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		result.Attempts = attempt

		val, err := op(ctx)
		if err == nil {
			result.Value = val
			return result, nil
		}

		// Don't retry if this is the last attempt
		if attempt == cfg.MaxAttempts {
			return result, err
		}

		// Don't retry non-retryable errors
		if !isRetryableRedisError(err) {
			return result, err
		}

		// Log the retry attempt
		log.Warn("redis_operation_retry",
			"attempt", attempt,
			"max_attempts", cfg.MaxAttempts,
			"backoff", backoff,
			"error", err.Error(),
		)

		// Wait with backoff
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("retry cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}

		result.TotalDelay += backoff
		backoff = time.Duration(float64(backoff) * cfg.BackoffMultiplier)
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}

	// This should never be reached, but just in case
	return result, fmt.Errorf("retry exhausted after %d attempts", result.Attempts)
}

// WithRetryVoid is a convenience wrapper for operations that don't return a value.
func WithRetryVoid(ctx context.Context, cfg RetryConfig, op func(ctx context.Context) error) (RetryResult[struct{}], error) {
	return WithRetry(ctx, cfg, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, op(ctx)
	})
}
