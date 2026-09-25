package websearch

import (
	"sync"
	"sync/atomic"
	"time"
)

// CircuitState represents the state of a circuit breaker
type CircuitState uint32

const (
	StateClosed   CircuitState = 0 // Normal operation
	StateOpen     CircuitState = 1 // Circuit open, requests blocked
	StateHalfOpen CircuitState = 2 // Testing recovery
)

// CircuitBreakerConfig defines circuit breaker parameters
type CircuitBreakerConfig struct {
	FailureThreshold    int           `json:"failure_threshold"`    // 0-100, percentage of failures to trigger open
	OpenTimeout         time.Duration `json:"open_timeout"`         // Wait time before half-open
	HalfOpenMaxRequests int           `json:"half_open_max_requests"` // Probe requests in half-open
	WindowSize          int           `json:"window_size"`          // Sliding window size (requests)
	WindowDuration      time.Duration `json:"window_duration"`      // Window time dimension
}

// DefaultCircuitBreakerConfig returns sensible defaults
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold:    50,
		OpenTimeout:         60 * time.Second,
		HalfOpenMaxRequests: 3,
		WindowSize:          100,
		WindowDuration:      5 * time.Minute,
	}
}

// windowEntry records a single request outcome
type windowEntry struct {
	success   bool
	timestamp time.Time
}

// CircuitBreaker implements the circuit breaker pattern with sliding window
type CircuitBreaker struct {
	state           atomic.Uint32
	stateMu         sync.Mutex // Only used during state transitions (CAS semantics)
	window          []windowEntry
	windowMu        sync.Mutex
	lastFailureNano atomic.Int64 // UnixNano, atomic to avoid data race
	config          CircuitBreakerConfig
	providerName    string
	callbacksMu     sync.RWMutex
	onStateChange   []CircuitBreakerCallback
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(cfg CircuitBreakerConfig, providerName string) *CircuitBreaker {
	return &CircuitBreaker{
		config:       cfg,
		providerName: providerName,
	}
}

// Allow checks if a request is allowed
func (cb *CircuitBreaker) Allow() bool {
	state := CircuitState(cb.state.Load())

	switch state {
	case StateClosed:
		return true
	case StateOpen:
		cb.stateMu.Lock()
		defer cb.stateMu.Unlock()
		if CircuitState(cb.state.Load()) != StateOpen {
			return false
		}
		lastFail := time.Unix(0, cb.lastFailureNano.Load())
		if time.Since(lastFail) > cb.config.OpenTimeout {
			cb.transitionState(StateHalfOpen, "open timeout expired")
			cb.resetWindowLocked()
			return true
		}
		return false
	case StateHalfOpen:
		cb.windowMu.Lock()
		successCount := cb.countSuccessInWindowLocked()
		cb.windowMu.Unlock()
		return successCount < cb.config.HalfOpenMaxRequests
	}
	return false
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	cb.recordOutcome(true)
}

// RecordFailure records a failed request
func (cb *CircuitBreaker) RecordFailure() {
	cb.recordOutcome(false)
}

// recordOutcome unified sliding window outcome recording
func (cb *CircuitBreaker) recordOutcome(success bool) {
	now := time.Now()

	cb.windowMu.Lock()
	cb.window = append(cb.window, windowEntry{success: success, timestamp: now})
	cb.window = cb.trimWindow(cb.window, now)
	failureRate := cb.calculateFailureRateLocked()
	cb.windowMu.Unlock()

	if !success {
		cb.lastFailureNano.Store(now.UnixNano())
	}

	cb.stateMu.Lock()
	defer cb.stateMu.Unlock()

	state := CircuitState(cb.state.Load())
	switch state {
	case StateHalfOpen:
		if success {
			cb.windowMu.Lock()
			successCount := cb.countSuccessInWindowLocked()
			cb.windowMu.Unlock()

			if successCount >= cb.config.HalfOpenMaxRequests {
				cb.transitionState(StateClosed, "half-open probe recovery")
				cb.resetWindow()
			}
		} else {
			cb.transitionState(StateOpen, "half-open probe failure")
		}
	case StateClosed:
		if failureRate >= float64(cb.config.FailureThreshold) {
			cb.transitionState(StateOpen, "failure threshold exceeded")
		}
	}
}

// countSuccessInWindowLocked counts successes in window (caller must hold windowMu)
func (cb *CircuitBreaker) countSuccessInWindowLocked() int {
	count := 0
	for _, e := range cb.window {
		if e.success {
			count++
		}
	}
	return count
}

// resetWindowLocked resets the sliding window (caller must hold windowMu)
func (cb *CircuitBreaker) resetWindowLocked() {
	cb.window = cb.window[:0]
}

// resetWindow resets the sliding window with lock
func (cb *CircuitBreaker) resetWindow() {
	cb.windowMu.Lock()
	defer cb.windowMu.Unlock()
	cb.resetWindowLocked()
}

// trimWindow cleans expired entries (caller must hold windowMu)
func (cb *CircuitBreaker) trimWindow(window []windowEntry, now time.Time) []windowEntry {
	if cb.config.WindowDuration > 0 {
		cutoff := now.Add(-cb.config.WindowDuration)
		start := 0
		for start < len(window) && window[start].timestamp.Before(cutoff) {
			start++
		}
		window = window[start:]
	}

	if len(window) > cb.config.WindowSize {
		window = window[len(window)-cb.config.WindowSize:]
	}

	return window
}

// transitionState unified state transition entry (triggers callbacks and metrics)
func (cb *CircuitBreaker) transitionState(newState CircuitState, reason string) {
	oldState := CircuitState(cb.state.Load())
	if oldState == newState {
		return
	}

	cb.state.Store(uint32(newState))

	event := CircuitBreakerEvent{
		ProviderName: cb.providerName,
		OldState:     oldState,
		NewState:     newState,
		Timestamp:    time.Now(),
		Reason:       reason,
	}

	cb.callbacksMu.RLock()
	callbacks := make([]CircuitBreakerCallback, len(cb.onStateChange))
	copy(callbacks, cb.onStateChange)
	cb.callbacksMu.RUnlock()

	for _, callback := range callbacks {
		go callback(event)
	}
}

// calculateFailureRateLocked calculates failure rate (0-100) within window
// Caller must hold windowMu
func (cb *CircuitBreaker) calculateFailureRateLocked() float64 {
	if len(cb.window) == 0 {
		return 0
	}
	failures := 0
	for _, e := range cb.window {
		if !e.success {
			failures++
		}
	}
	return float64(failures) * 100 / float64(len(cb.window))
}

// CircuitBreakerEvent represents a state change event
type CircuitBreakerEvent struct {
	ProviderName string
	OldState     CircuitState
	NewState     CircuitState
	Timestamp    time.Time
	Reason       string
}

// CircuitBreakerCallback is called on state changes
type CircuitBreakerCallback func(event CircuitBreakerEvent)

// OnStateChange registers a state change callback
func (cb *CircuitBreaker) OnStateChange(callback CircuitBreakerCallback) {
	cb.callbacksMu.Lock()
	defer cb.callbacksMu.Unlock()
	cb.onStateChange = append(cb.onStateChange, callback)
}
