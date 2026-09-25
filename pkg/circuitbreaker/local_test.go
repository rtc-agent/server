package circuitbreaker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDefaultCircuitBreakerConfig(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()

	if cfg.FailureThreshold != 50 {
		t.Errorf("expected FailureThreshold 50, got %d", cfg.FailureThreshold)
	}
	if cfg.OpenTimeout != 60*time.Second {
		t.Errorf("expected OpenTimeout 60s, got %v", cfg.OpenTimeout)
	}
	if cfg.HalfOpenMaxRequests != 3 {
		t.Errorf("expected HalfOpenMaxRequests 3, got %d", cfg.HalfOpenMaxRequests)
	}
	if cfg.WindowSize != 100 {
		t.Errorf("expected WindowSize 100, got %d", cfg.WindowSize)
	}
	if cfg.WindowDuration != 5*time.Minute {
		t.Errorf("expected WindowDuration 5m, got %v", cfg.WindowDuration)
	}
}

func TestNewCircuitBreaker(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	cb := NewCircuitBreaker(cfg, "test-provider")

	if cb == nil {
		t.Fatal("expected non-nil CircuitBreaker")
	}
	if cb.providerName != "test-provider" {
		t.Errorf("expected provider name 'test-provider', got '%s'", cb.providerName)
	}
	if cb.State() != StateClosed {
		t.Errorf("expected initial state Closed, got %v", cb.State())
	}
}

func TestCircuitBreaker_Allow(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	// Initially closed, should allow
	if !cb.Allow() {
		t.Error("expected Allow() to return true in Closed state")
	}

	// Force open state by recording failures
	for i := 0; i < 10; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	if !cb.IsOpen() {
		t.Error("expected circuit to be open after many failures")
	}

	// In open state, should not allow
	if cb.Allow() {
		t.Error("expected Allow() to return false in Open state")
	}
}

func TestCircuitBreaker_Execute_Success(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	cb := NewCircuitBreaker(cfg, "test")

	result, err := cb.Execute(func() (any, error) {
		return "success", nil
	})

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if result != "success" {
		t.Errorf("expected result 'success', got %v", result)
	}
}

func TestCircuitBreaker_Execute_Failure(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	cb := NewCircuitBreaker(cfg, "test")

	expectedErr := errors.New("test error")
	result, err := cb.Execute(func() (any, error) {
		return nil, expectedErr
	})

	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

func TestCircuitBreaker_RecordSuccess(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	cb := NewCircuitBreaker(cfg, "test")

	// Should not panic
	cb.RecordSuccess()

	// Circuit should still be closed
	if cb.State() != StateClosed {
		t.Errorf("expected state Closed after success, got %v", cb.State())
	}
}

func TestCircuitBreaker_RecordFailure(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	// First add some successes to avoid tripping on first failure
	for i := 0; i < 3; i++ {
		cb.RecordSuccess()
	}

	// Should not panic
	cb.RecordFailure()

	// Circuit should still be closed (1 failure out of 4 = 25% < 50%)
	if cb.State() != StateClosed {
		t.Errorf("expected state Closed after single failure with prior successes, got %v", cb.State())
	}
}

func TestCircuitBreaker_IsOpen(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	// Initially not open
	if cb.IsOpen() {
		t.Error("expected IsOpen() to return false initially")
	}

	// Trip the circuit
	for i := 0; i < 20; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	// Should be open now
	if !cb.IsOpen() {
		t.Error("expected IsOpen() to return true after tripping")
	}
}

func TestCircuitBreaker_StateTransition(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         100 * time.Millisecond,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	// Initial state: Closed
	if cb.State() != StateClosed {
		t.Errorf("expected initial state Closed, got %v", cb.State())
	}

	// Trip to Open
	for i := 0; i < 20; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	if cb.State() != StateOpen {
		t.Errorf("expected state Open after failures, got %v", cb.State())
	}

	// Wait for timeout to transition to HalfOpen
	time.Sleep(150 * time.Millisecond)

	// State should be HalfOpen now (gobreaker checks timeout internally)
	// Try to execute to trigger the transition
	cb.Execute(func() (any, error) {
		return "test", nil
	})

	// After successful probe, should transition back to Closed
	// Note: gobreaker's behavior depends on MaxRequests and success/failure
}

func TestCircuitBreaker_ReadyToTrip(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    60, // 60% failure rate
		OpenTimeout:         1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	// 3 successes, then 7 failures = 70% failure rate > 60% threshold
	for i := 0; i < 3; i++ {
		cb.Execute(func() (any, error) {
			return "ok", nil
		})
	}

	// Should still be closed
	if cb.IsOpen() {
		t.Error("expected circuit to remain closed at 0% failure rate")
	}

	// Add failures to exceed threshold
	for i := 0; i < 7; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	// Should be open now (70% > 60%)
	if !cb.IsOpen() {
		t.Error("expected circuit to open at 70% failure rate")
	}
}

func TestCircuitBreaker_OnStateChange(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         100 * time.Millisecond,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	var events []CircuitBreakerEvent
	var mu sync.Mutex

	cb.OnStateChange(func(event CircuitBreakerEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})

	// Trip the circuit to trigger state change
	for i := 0; i < 20; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	// Wait for callback to be invoked (it's async)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	eventCount := len(events)
	mu.Unlock()

	if eventCount == 0 {
		t.Error("expected at least one state change event")
	}

	// Check that we have a transition from Closed to Open
	foundTransition := false
	for _, e := range events {
		if e.OldState == StateClosed && e.NewState == StateOpen {
			foundTransition = true
			break
		}
	}

	if !foundTransition {
		t.Error("expected Closed -> Open transition event")
	}
}

func TestCircuitBreaker_Execute_OpenState(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	// Trip the circuit
	for i := 0; i < 20; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	// Try to execute in open state
	result, err := cb.Execute(func() (any, error) {
		return "should not run", nil
	})

	// Should get an error (ErrOpenState or similar)
	if err == nil {
		t.Error("expected error when executing in open state")
	}
	if result != nil {
		t.Errorf("expected nil result in open state, got %v", result)
	}
}

func TestCircuitBreaker_MultipleCallbacks(t *testing.T) {
	cfg := CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         1 * time.Second,
		HalfOpenMaxRequests: 2,
		WindowSize:          10,
		WindowDuration:      1 * time.Minute,
	}
	cb := NewCircuitBreaker(cfg, "test")

	callback1Called := false
	callback2Called := false

	cb.OnStateChange(func(event CircuitBreakerEvent) {
		callback1Called = true
	})

	cb.OnStateChange(func(event CircuitBreakerEvent) {
		callback2Called = true
	})

	// Trip the circuit
	for i := 0; i < 20; i++ {
		cb.Execute(func() (any, error) {
			return nil, errors.New("fail")
		})
	}

	time.Sleep(50 * time.Millisecond)

	if !callback1Called {
		t.Error("expected callback1 to be called")
	}
	if !callback2Called {
		t.Error("expected callback2 to be called")
	}
}
