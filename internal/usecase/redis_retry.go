// Package usecase provides business logic implementations.
package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// redisRetryConfig configures retry behavior for critical Redis operations.
const (
	// redisRetryMaxAttempts is the maximum number of attempts for Redis operations.
	redisRetryMaxAttempts = 3

	// redisRetryInitialBackoff is the initial backoff duration.
	redisRetryInitialBackoff = 100 * time.Millisecond

	// redisRetryMaxBackoff is the maximum backoff duration.
	redisRetryMaxBackoff = 1 * time.Second
)

// isRetryableRedisError returns true if the error is a transient network error.
func isRetryableRedisError(err error) bool {
	if err == nil {
		return false
	}

	// Never retry context errors
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Never retry redis.Nil - it's an expected "not found" result
	if errors.Is(err, redis.Nil) {
		return false
	}

	// Check for common transient network errors
	errMsg := err.Error()
	return containsAny(errMsg,
		"connection refused",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"EOF",
		"no such host",
		"LOADING",
		"CLUSTERDOWN",
		"TRYAGAIN",
		"BUSY",
		"READONLY",
	)
}

// containsAny returns true if s contains any of the substrings.
func containsAny(s string, substrs ...string) bool {
	for _, substr := range substrs {
		// Handle case where substr equals s exactly
		if s == substr {
			return true
		}
		// Handle case where substr is longer than s
		if len(s) < len(substr) {
			continue
		}
		// Substring search
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
	}
	return false
}

// withRedisRetry executes the given operation with retry logic for transient errors.
func withRedisRetry[T any](ctx context.Context, op string, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	backoff := redisRetryInitialBackoff

	for attempt := 1; attempt <= redisRetryMaxAttempts; attempt++ {
		val, err := fn(ctx)
		if err == nil {
			return val, nil
		}

		// Don't retry if this is the last attempt
		if attempt == redisRetryMaxAttempts {
			return zero, err
		}

		// Don't retry non-retryable errors
		if !isRetryableRedisError(err) {
			return zero, err
		}

		// Log the retry attempt
		logger.Warn(ctx, "redis_operation_retry",
			zap.String("operation", op),
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", redisRetryMaxAttempts),
			zap.Duration("backoff", backoff),
			zap.Error(err),
		)

		// Wait with backoff
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > redisRetryMaxBackoff {
			backoff = redisRetryMaxBackoff
		}
	}

	return zero, nil
}
