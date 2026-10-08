package httphandler

import (
	"errors"
	"testing"
)

// TestIsTransientRedisError tests the detection of transient Redis errors.
func TestIsTransientRedisError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "connection reset",
			err:      errors.New("connection reset by peer"),
			expected: true,
		},
		{
			name:     "i/o timeout",
			err:      errors.New("i/o timeout"),
			expected: true,
		},
		{
			name:     "NOSCRIPT",
			err:      errors.New("NOSCRIPT No matching script"),
			expected: true,
		},
		{
			name:     "permanent error - quota exceeded",
			err:      errors.New("quota exceeded"),
			expected: false,
		},
		{
			name:     "permanent error - not found",
			err:      errors.New("key not found"),
			expected: false,
		},
		{
			name:     "wrapped transient error",
			err:      errors.New("redis command failed: connection reset"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isTransientRedisError(tt.err)
			if result != tt.expected {
				t.Errorf("isTransientRedisError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

// TestCommitQuotaWithRetry_Integration is a placeholder for integration tests.
// Full retry logic testing requires a real Redis instance to verify:
// 1. Exponential backoff timing
// 2. Idempotency via SETNX
// 3. Context cancellation behavior
// 4. Metrics recording (RecordQuotaCommitRetry)
//
// Run with: go test -tags=integration -run TestCommitQuotaWithRetry_Integration
func TestCommitQuotaWithRetry_Integration(t *testing.T) {
	t.Skip("Integration test requires Redis - use Docker Compose for full testing")
}
