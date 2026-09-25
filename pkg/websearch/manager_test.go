package websearch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rtc-agent/server/pkg/circuitbreaker"
)

// MockProvider implements WebSearchProvider for testing
type MockProvider struct {
	name     string
	response *SearchResponse
	err      error
	delay    time.Duration
}

func (m *MockProvider) Name() string { return m.name }

func (m *MockProvider) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.response, m.err
}

func (m *MockProvider) HealthCheck(ctx context.Context) error { return m.err }
func (m *MockProvider) Close() error                          { return nil }

// Export MockProvider for use in examples
var _ = MockProvider{}

func TestRoundRobinBalancer(t *testing.T) {
	balancer := NewRoundRobinBalancer()

	providers := []WebSearchProvider{
		&MockProvider{name: "p1"},
		&MockProvider{name: "p2"},
		&MockProvider{name: "p3"},
	}

	// Test round-robin selection
	selected := make(map[string]int)
	for i := 0; i < 9; i++ {
		p, err := balancer.Select(context.Background(), providers)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selected[p.Name()]++
	}

	// Each provider should be selected 3 times
	for _, count := range selected {
		if count != 3 {
			t.Errorf("expected 3 selections per provider, got %v", selected)
			break
		}
	}

	// Test empty list
	_, err := balancer.Select(context.Background(), nil)
	if err == nil {
		t.Error("expected error for empty provider list")
	}
}

func TestWeightedRoundRobinBalancer(t *testing.T) {
	weights := map[string]int{
		"high":   70,
		"medium": 20,
		"low":    10,
	}
	balancer := NewWeightedRandomBalancer(weights)

	providers := []WebSearchProvider{
		&MockProvider{name: "high"},
		&MockProvider{name: "medium"},
		&MockProvider{name: "low"},
	}

	// Test weighted selection
	selected := make(map[string]int)
	iterations := 1000
	for i := 0; i < iterations; i++ {
		p, err := balancer.Select(context.Background(), providers)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selected[p.Name()]++
	}

	// Verify distribution roughly matches weights (with tolerance)
	highRatio := float64(selected["high"]) / float64(iterations)
	if highRatio < 0.6 || highRatio > 0.8 {
		t.Errorf("high weight provider ratio %.2f outside expected range [0.6, 0.8]", highRatio)
	}
}

func TestCircuitBreaker(t *testing.T) {
	cfg := circuitbreaker.CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         100 * time.Millisecond,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Second,
	}

	cb := circuitbreaker.NewCircuitBreaker(cfg, "test-provider")

	// Initial state: closed, should allow
	if !cb.Allow() {
		t.Error("expected Allow() = true in closed state")
	}

	// Record failures to trigger open (need at least 10 requests for ReadyToTrip)
	for i := 0; i < 12; i++ {
		cb.RecordFailure()
	}

	// Should be open now
	if cb.Allow() {
		t.Error("expected Allow() = false after threshold exceeded")
	}

	// Wait for timeout
	time.Sleep(150 * time.Millisecond)

	// Should transition to half-open
	if !cb.Allow() {
		t.Error("expected Allow() = true after timeout")
	}

	// Record successes to recover
	cb.RecordSuccess()
	cb.RecordSuccess()

	// Should be closed again
	if !cb.Allow() {
		t.Error("expected Allow() = true after recovery")
	}
}

func TestRateLimiter(t *testing.T) {
	limiter := NewRateLimiter(10, 5) // 10 req/s, burst 5

	// Should allow burst
	for i := 0; i < 5; i++ {
		if !limiter.Allow() {
			t.Errorf("expected Allow() = true for burst request %d", i)
		}
	}

	// 6th request should be blocked (burst exhausted)
	if limiter.Allow() {
		t.Error("expected Allow() = false after burst exhausted")
	}

	// Wait for token replenishment
	time.Sleep(150 * time.Millisecond) // Should get 1-2 tokens

	if !limiter.Allow() {
		t.Error("expected Allow() = true after waiting")
	}
}

func TestWebSearchManager(t *testing.T) {
	providers := []WebSearchProvider{
		&MockProvider{
			name: "p1",
			response: &SearchResponse{
				Results: []SearchResult{
					{Title: "Result 1", URL: "http://example.com/1"},
				},
				Provider: "p1",
			},
		},
		&MockProvider{
			name: "p2",
			response: &SearchResponse{
				Results: []SearchResult{
					{Title: "Result 2", URL: "http://example.com/2"},
				},
				Provider: "p2",
			},
		},
	}

	cfg := DefaultWebSearchConfig()
	manager, err := NewWebSearchManager(cfg, providers, nil)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx := context.Background()
	req := &SearchRequest{
		Query:      "test query",
		MaxResults: 10,
	}

	// Test successful search
	resp, err := manager.Search(ctx, req)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(resp.Results) == 0 {
		t.Error("expected at least one result")
	}

	// Test with failing provider
	failProvider := &MockProvider{
		name: "failing",
		err:  errors.New("search error"),
	}

	cfg2 := DefaultWebSearchConfig()
	manager2, _ := NewWebSearchManager(cfg2, []WebSearchProvider{failProvider}, nil)

	_, err = manager2.Search(ctx, req)
	if err == nil {
		t.Error("expected error from failing provider")
	}
}

func TestWebSearchManagerLifecycle(t *testing.T) {
	providers := []WebSearchProvider{
		&MockProvider{name: "p1", response: &SearchResponse{}},
	}

	cfg := DefaultWebSearchConfig()
	manager, _ := NewWebSearchManager(cfg, providers, nil)

	ctx := context.Background()

	// Start
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Health check
	if err := manager.HealthCheck(ctx); err != nil {
		t.Fatalf("health check failed: %v", err)
	}

	// Stop
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	// Search should fail after stop
	_, err := manager.Search(ctx, &SearchRequest{Query: "test"})
	if err == nil {
		t.Error("expected error after stop")
	}
}
