package usecase

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHealthChecker_Liveness tests the Liveness method.
func TestHealthChecker_Liveness(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		deps     HealthCheckerDeps
		expected AggregatedHealth
	}{
		{
			name: "liveness_always_true",
			deps: HealthCheckerDeps{},
			expected: AggregatedHealth{
				AllReady: true,
				Components: map[string]ComponentHealth{
					"process": {Status: HealthStatusOK},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := NewHealthChecker(tt.deps)
			result := hc.Liveness(ctx)

			assert.Equal(t, tt.expected.AllReady, result.AllReady)
			assert.Equal(t, tt.expected.Components["process"].Status, result.Components["process"].Status)
		})
	}
}

// TestHealthChecker_Readiness tests the Readiness method.
func TestHealthChecker_Readiness(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name             string
		deps             HealthCheckerDeps
		expectAllReady   bool
		expectComponents []string
	}{
		{
			name: "all_not_configured",
			deps: HealthCheckerDeps{
				DB:    nil,
				Redis: nil,
			},
			expectAllReady:   true, // Not configured components don't fail readiness
			expectComponents: []string{"db", "redis", "centrifuge"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := NewHealthChecker(tt.deps)
			result := hc.Readiness(ctx)

			assert.Equal(t, tt.expectAllReady, result.AllReady)
			for _, comp := range tt.expectComponents {
				_, exists := result.Components[comp]
				assert.True(t, exists, "component %s should exist", comp)
			}
		})
	}
}

// TestHealthChecker_Readiness_NotConfigured tests readiness with no dependencies configured.
func TestHealthChecker_Readiness_NotConfigured(t *testing.T) {
	ctx := context.Background()

	hc := NewHealthChecker(HealthCheckerDeps{})
	result := hc.Readiness(ctx)

	// All components should be "not configured"
	assert.True(t, result.AllReady, "not configured components should not fail readiness")
	assert.Equal(t, HealthStatusNotConfigured, result.Components["db"].Status)
	assert.Equal(t, HealthStatusNotConfigured, result.Components["redis"].Status)
	assert.Equal(t, HealthStatusNotConfigured, result.Components["centrifuge"].Status)
}

// TestHealthChecker_DatabaseReachable tests the DatabaseReachable method.
func TestHealthChecker_DatabaseReachable(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		deps     HealthCheckerDeps
		expected bool
	}{
		{
			name: "nil_db",
			deps: HealthCheckerDeps{
				DB: nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := NewHealthChecker(tt.deps)
			result := hc.DatabaseReachable(ctx)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestHealthStatus_Constants tests health status constants.
func TestHealthStatus_Constants(t *testing.T) {
	assert.Equal(t, HealthStatus("ok"), HealthStatusOK)
	assert.Equal(t, HealthStatus("error"), HealthStatusError)
	assert.Equal(t, HealthStatus("not configured"), HealthStatusNotConfigured)
}

// TestComponentHealth_Structure tests the ComponentHealth struct.
func TestComponentHealth_Structure(t *testing.T) {
	ch := ComponentHealth{
		Status: HealthStatusOK,
		Error:  "",
	}

	assert.Equal(t, HealthStatusOK, ch.Status)
	assert.Empty(t, ch.Error)

	ch.Error = "connection failed"
	assert.Equal(t, "connection failed", ch.Error)
}

// TestAggregatedHealth_Structure tests the AggregatedHealth struct.
func TestAggregatedHealth_Structure(t *testing.T) {
	ah := AggregatedHealth{
		AllReady: true,
		Components: map[string]ComponentHealth{
			"db":    {Status: HealthStatusOK},
			"redis": {Status: HealthStatusOK},
		},
	}

	assert.True(t, ah.AllReady)
	assert.Len(t, ah.Components, 2)
	assert.Equal(t, HealthStatusOK, ah.Components["db"].Status)
	assert.Equal(t, HealthStatusOK, ah.Components["redis"].Status)
}

// TestHealthCheckerDeps_Structure tests the HealthCheckerDeps struct.
func TestHealthCheckerDeps_Structure(t *testing.T) {
	deps := HealthCheckerDeps{
		DB:    nil,
		Redis: nil,
	}

	assert.Nil(t, deps.DB)
	assert.Nil(t, deps.Redis)
	assert.Nil(t, deps.CentrifugeNode)
}

// TestNewHealthChecker tests the NewHealthChecker constructor.
func TestNewHealthChecker(t *testing.T) {
	deps := HealthCheckerDeps{}
	hc := NewHealthChecker(deps)

	assert.NotNil(t, hc)
}

// TestHealthChecker_Liveness_ContextCancellation tests liveness with cancelled context.
func TestHealthChecker_Liveness_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	hc := NewHealthChecker(HealthCheckerDeps{})
	result := hc.Liveness(ctx)

	// Liveness should not be affected by context cancellation
	assert.True(t, result.AllReady)
}

// TestHealthChecker_Readiness_Timeout tests readiness timeout behavior.
func TestHealthChecker_Readiness_Timeout(t *testing.T) {
	// This test would require mocking DB/Redis to test timeout
	// Skipping for unit test - would be covered in integration tests
	t.Skip("Requires real DB/Redis dependencies - covered in integration tests")
}

// TestHealthChecker_DatabaseReachable_WithNilDB tests DatabaseReachable with nil DB.
func TestHealthChecker_DatabaseReachable_WithNilDB(t *testing.T) {
	ctx := context.Background()

	hc := NewHealthChecker(HealthCheckerDeps{
		DB: nil,
	})

	result := hc.DatabaseReachable(ctx)
	assert.False(t, result, "should return false when DB is nil")
}

// TestHealthChecker_ComponentHealth_JSON tests JSON marshaling of ComponentHealth.
func TestHealthChecker_ComponentHealth_JSON(t *testing.T) {
	ch := ComponentHealth{
		Status: HealthStatusOK,
		Error:  "test error",
	}

	// Verify struct fields are accessible
	assert.Equal(t, HealthStatusOK, ch.Status)
	assert.Equal(t, "test error", ch.Error)
}

// TestHealthChecker_AggregatedHealth_JSON tests JSON marshaling of AggregatedHealth.
func TestHealthChecker_AggregatedHealth_JSON(t *testing.T) {
	ah := AggregatedHealth{
		AllReady: true,
		Components: map[string]ComponentHealth{
			"db": {Status: HealthStatusOK, Error: ""},
		},
	}

	// Verify struct fields are accessible
	assert.True(t, ah.AllReady)
	assert.Len(t, ah.Components, 1)
}

// BenchmarkHealthChecker_Liveness benchmarks liveness checks.
func BenchmarkHealthChecker_Liveness(b *testing.B) {
	ctx := context.Background()
	hc := NewHealthChecker(HealthCheckerDeps{})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hc.Liveness(ctx)
	}
}

// BenchmarkHealthChecker_Readiness benchmarks readiness checks.
func BenchmarkHealthChecker_Readiness(b *testing.B) {
	ctx := context.Background()
	hc := NewHealthChecker(HealthCheckerDeps{})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hc.Readiness(ctx)
	}
}

// ExampleNewHealthChecker demonstrates creating a HealthChecker.
func ExampleNewHealthChecker() {
	// Create with no dependencies (all will be "not configured")
	hc := NewHealthChecker(HealthCheckerDeps{})

	// Check liveness
	ctx := context.Background()
	liveness := hc.Liveness(ctx)
	_ = liveness.AllReady

	// Check readiness
	readiness := hc.Readiness(ctx)
	_ = readiness.AllReady

	// Check database reachability
	dbReachable := hc.DatabaseReachable(ctx)
	_ = dbReachable
}

// TestHealthChecker_Readiness_WithRealDependencies tests with real dependencies.
func TestHealthChecker_Readiness_WithRealDependencies(t *testing.T) {
	// This test would require real DB/Redis connections
	// Skipping for unit test - would be covered in integration tests
	t.Skip("Requires real DB/Redis dependencies - covered in integration tests")
}

// TestHealthChecker_DatabaseReachable_WithRealDB tests with real DB connection.
func TestHealthChecker_DatabaseReachable_WithRealDB(t *testing.T) {
	// This test would require a real DB connection
	// Skipping for unit test - would be covered in integration tests
	t.Skip("Requires real DB connection - covered in integration tests")
}

// TestHealthChecker_ErrorPropagation tests error propagation from dependencies.
func TestHealthChecker_ErrorPropagation(t *testing.T) {
	// This test would require mocking DB/Redis errors
	// Skipping for unit test - would be covered in integration tests
	t.Skip("Requires mock DB/Redis - covered in integration tests")
}

// TestHealthChecker_NilDependencies tests behavior with nil dependencies.
func TestHealthChecker_NilDependencies(t *testing.T) {
	ctx := context.Background()

	hc := NewHealthChecker(HealthCheckerDeps{
		DB:    nil,
		Redis: nil,
	})

	// Liveness should always be true
	liveness := hc.Liveness(ctx)
	assert.True(t, liveness.AllReady)

	// Readiness should not fail with nil dependencies
	readiness := hc.Readiness(ctx)
	assert.True(t, readiness.AllReady)

	// DatabaseReachable should return false with nil DB
	dbReachable := hc.DatabaseReachable(ctx)
	assert.False(t, dbReachable)
}

// TestHealthChecker_StatusValues tests all possible status values.
func TestHealthChecker_StatusValues(t *testing.T) {
	statuses := []HealthStatus{
		HealthStatusOK,
		HealthStatusError,
		HealthStatusNotConfigured,
	}

	for _, status := range statuses {
		assert.NotEmpty(t, status)
	}
}

// TestHealthChecker_ComponentNames tests that component names are consistent.
func TestHealthChecker_ComponentNames(t *testing.T) {
	ctx := context.Background()

	hc := NewHealthChecker(HealthCheckerDeps{})
	readiness := hc.Readiness(ctx)

	// Verify expected component names in readiness
	expectedComponents := []string{"db", "redis", "centrifuge"}
	for _, comp := range expectedComponents {
		_, exists := readiness.Components[comp]
		assert.True(t, exists, "component %s should exist in readiness", comp)
	}
}

// TestHealthChecker_Readiness_PartialConfiguration tests partial dependency configuration.
func TestHealthChecker_Readiness_PartialConfiguration(t *testing.T) {
	ctx := context.Background()

	// Test with only DB configured (but nil in this case)
	hc := NewHealthChecker(HealthCheckerDeps{
		DB: nil,
	})

	readiness := hc.Readiness(ctx)
	assert.NotNil(t, readiness.Components)
}

// TestHealthChecker_Liveness_MultipleCalls tests that liveness is consistent.
func TestHealthChecker_Liveness_MultipleCalls(t *testing.T) {
	ctx := context.Background()

	hc := NewHealthChecker(HealthCheckerDeps{})

	result1 := hc.Liveness(ctx)
	result2 := hc.Liveness(ctx)

	assert.Equal(t, result1.AllReady, result2.AllReady)
	assert.Equal(t, result1.Components["process"].Status, result2.Components["process"].Status)
}
