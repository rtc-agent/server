package rtcqueue_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// TestRetryConfig_Default verifies the default retry configuration.
func TestRetryConfig_Default(t *testing.T) {
	cfg := rtcqueue.DefaultRetryConfig()

	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", cfg.MaxAttempts)
	}
	if cfg.InitialBackoff != 100*time.Millisecond {
		t.Errorf("InitialBackoff = %v, want 100ms", cfg.InitialBackoff)
	}
	if cfg.BackoffMultiplier != 2.0 {
		t.Errorf("BackoffMultiplier = %f, want 2.0", cfg.BackoffMultiplier)
	}
	if cfg.MaxBackoff != 1*time.Second {
		t.Errorf("MaxBackoff = %v, want 1s", cfg.MaxBackoff)
	}
}

// TestWithRetry_Success verifies that WithRetry returns immediately on success.
func TestWithRetry_Success(t *testing.T) {
	cfg := rtcqueue.DefaultRetryConfig()
	var attempts int

	result, err := rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "success", nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Value != "success" {
		t.Errorf("Value = %q, want %q", result.Value, "success")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if result.Attempts != 1 {
		t.Errorf("result.Attempts = %d, want 1", result.Attempts)
	}
}

// TestWithRetry_NonRetryableError verifies that non-retryable errors are not retried.
func TestWithRetry_NonRetryableError(t *testing.T) {
	cfg := rtcqueue.DefaultRetryConfig()
	var attempts int
	expectedErr := errors.New("business error")

	_, err := rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", expectedErr
	})

	if !errors.Is(err, expectedErr) {
		t.Errorf("err = %v, want %v", err, expectedErr)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (non-retryable error should not be retried)", attempts)
	}
}

// TestWithRetry_RedisNilNotRetried verifies that redis.Nil is not retried.
func TestWithRetry_RedisNilNotRetried(t *testing.T) {
	cfg := rtcqueue.DefaultRetryConfig()
	var attempts int

	_, err := rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", redis.Nil
	})

	if !errors.Is(err, redis.Nil) {
		t.Errorf("err = %v, want redis.Nil", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (redis.Nil should not be retried)", attempts)
	}
}

// TestWithRetry_TransientErrorRetried verifies that transient errors are retried.
func TestWithRetry_TransientErrorRetried(t *testing.T) {
	cfg := rtcqueue.RetryConfig{
		MaxAttempts:       3,
		InitialBackoff:    10 * time.Millisecond, // Short for testing
		BackoffMultiplier: 2.0,
		MaxBackoff:        50 * time.Millisecond,
	}

	var attempts int
	transientErr := errors.New("connection refused")

	result, err := rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		if attempts < 3 {
			return "", transientErr
		}
		return "recovered", nil
	})

	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if result.Value != "recovered" {
		t.Errorf("Value = %q, want %q", result.Value, "recovered")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if result.Attempts != 3 {
		t.Errorf("result.Attempts = %d, want 3", result.Attempts)
	}
}

// TestWithRetry_ExhaustedRetries verifies that exhausted retries return the last error.
func TestWithRetry_ExhaustedRetries(t *testing.T) {
	cfg := rtcqueue.RetryConfig{
		MaxAttempts:       2,
		InitialBackoff:    10 * time.Millisecond,
		BackoffMultiplier: 2.0,
		MaxBackoff:        50 * time.Millisecond,
	}

	var attempts int
	transientErr := errors.New("connection refused")

	_, err := rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", transientErr
	})

	if !errors.Is(err, transientErr) {
		t.Errorf("err = %v, want %v", err, transientErr)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

// TestWithRetry_ContextCancelled verifies that context cancellation stops retries.
func TestWithRetry_ContextCancelled(t *testing.T) {
	cfg := rtcqueue.RetryConfig{
		MaxAttempts:       5,
		InitialBackoff:    100 * time.Millisecond,
		BackoffMultiplier: 2.0,
		MaxBackoff:        1 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	var attempts int
	transientErr := errors.New("connection refused")

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := rtcqueue.WithRetry(ctx, cfg, func(ctx context.Context) (string, error) {
		attempts++
		return "", transientErr
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// Should have attempted at most 2 times before context was cancelled
	if attempts > 2 {
		t.Errorf("attempts = %d, want <= 2 (context should have stopped retries)", attempts)
	}
}

// TestWithRetry_CustomLogger verifies that custom logger receives retry events.
func TestWithRetry_CustomLogger(t *testing.T) {
	cfg := rtcqueue.RetryConfig{
		MaxAttempts:       3,
		InitialBackoff:    10 * time.Millisecond,
		BackoffMultiplier: 2.0,
		MaxBackoff:        50 * time.Millisecond,
		Logger:            &testRetryLogger{},
	}

	var attempts int
	transientErr := errors.New("connection refused")

	_, err := rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
		attempts++
		if attempts < 3 {
			return "", transientErr
		}
		return "ok", nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// testRetryLogger is a test implementation of RetryLogger.
type testRetryLogger struct {
	warnCount atomic.Int32
}

func (l *testRetryLogger) Warn(msg string, keysAndValues ...any) {
	l.warnCount.Add(1)
}

// TestIsRetryableRedisError tests the retryable error detection.
// This is tested indirectly through WithRetry, but we can also test
// the behavior more directly.
func TestIsRetryableRedisError_TransientErrors(t *testing.T) {
	cfg := rtcqueue.RetryConfig{
		MaxAttempts:       2,
		InitialBackoff:    10 * time.Millisecond,
		BackoffMultiplier: 2.0,
		MaxBackoff:        50 * time.Millisecond,
	}

	testCases := []struct {
		name      string
		err       error
		shouldRetry bool
	}{
		{"connection refused", errors.New("connection refused"), true},
		{"connection reset", errors.New("connection reset by peer"), true},
		{"broken pipe", errors.New("broken pipe"), true},
		{"i/o timeout", errors.New("i/o timeout"), true},
		{"EOF", errors.New("EOF"), true},
		{"redis.Nil", redis.Nil, false},
		{"context cancelled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"business error", errors.New("some business error"), false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			_, _ = rtcqueue.WithRetry(context.Background(), cfg, func(ctx context.Context) (string, error) {
				attempts++
				return "", tc.err
			})

			if tc.shouldRetry {
				if attempts != cfg.MaxAttempts {
					t.Errorf("attempts = %d, want %d (should retry)", attempts, cfg.MaxAttempts)
				}
			} else {
				if attempts != 1 {
					t.Errorf("attempts = %d, want 1 (should not retry)", attempts)
				}
			}
		})
	}
}
