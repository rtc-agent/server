package circuitbreaker

import (
	"fmt"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
)

// CircuitState represents the state of a circuit breaker
type CircuitState = gobreaker.State

const (
	StateClosed   = gobreaker.StateClosed
	StateOpen     = gobreaker.StateOpen
	StateHalfOpen = gobreaker.StateHalfOpen
)

// CircuitBreakerConfig defines circuit breaker parameters
type CircuitBreakerConfig struct {
	FailureThreshold    int           `json:"failure_threshold"`      // 0-100, percentage of failures to trigger open
	OpenTimeout         time.Duration `json:"open_timeout"`           // Wait time before half-open
	HalfOpenMaxRequests int           `json:"half_open_max_requests"` // Probe requests in half-open
	WindowSize          int           `json:"window_size"`            // Sliding window size (requests)
	WindowDuration      time.Duration `json:"window_duration"`        // Window time dimension
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

// CircuitBreaker wraps sony/gobreaker with additional callback support
type CircuitBreaker struct {
	cb            *gobreaker.CircuitBreaker[any]
	providerName  string
	config        CircuitBreakerConfig
	callbacksMu   sync.RWMutex
	onStateChange []CircuitBreakerCallback
}

// NewCircuitBreaker creates a new circuit breaker using sony/gobreaker
func NewCircuitBreaker(cfg CircuitBreakerConfig, providerName string) *CircuitBreaker {
	cb := &CircuitBreaker{
		providerName: providerName,
		config:       cfg,
	}

	settings := gobreaker.Settings{
		Name:        providerName,
		MaxRequests: uint32(cfg.HalfOpenMaxRequests),
		Interval:    cfg.WindowDuration,
		Timeout:     cfg.OpenTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Trip when failure rate exceeds threshold
			// Minimum 10 requests to avoid premature tripping on low sample sizes
			requests := counts.Requests
			if requests < 10 {
				return false
			}
			failureRate := float64(counts.TotalFailures) * 100 / float64(requests)
			return failureRate >= float64(cfg.FailureThreshold)
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			event := CircuitBreakerEvent{
				ProviderName: name,
				OldState:     from,
				NewState:     to,
				Timestamp:    time.Now(),
				Reason:       "state transition",
			}
			cb.callbacksMu.RLock()
			callbacks := make([]CircuitBreakerCallback, len(cb.onStateChange))
			copy(callbacks, cb.onStateChange)
			cb.callbacksMu.RUnlock()

			for _, callback := range callbacks {
				go callback(event)
			}
		},
	}

	cb.cb = gobreaker.NewCircuitBreaker[any](settings)
	return cb
}

// Allow checks if a request is allowed (non-blocking check)
//
// SEMANTICS:
//   - Closed state: always returns true
//   - Open state: always returns false
//   - HalfOpen state: always returns true (but see note below)
//
// IMPORTANT: Allow() does NOT consume a slot in half-open state and does NOT
// trigger state transitions. It's a pure state read operation.
//
// For actual request execution, use Execute() which:
//   - Enforces MaxRequests limit in half-open state
//   - Triggers open→half-open transition when timeout expires
//   - Records success/failure outcomes
//
// Use Allow() only for:
//   - Monitoring/metrics (checking current state)
//   - Fast-path rejection before expensive setup
//   - Health checks
//
// Do NOT use Allow() as a gate before Execute() - Execute() already checks
// state internally and the two calls can race (TOCTOU).
func (cb *CircuitBreaker) Allow() bool {
	state := cb.cb.State()
	if state == gobreaker.StateClosed {
		return true
	}
	if state == gobreaker.StateOpen {
		return false
	}
	// Half-open: allow limited requests
	return true
}

// RecordSuccess records a successful request
// For backward compatibility - wraps Execute with a successful operation
//
// LIMITATION: When the circuit is Open, Execute() returns ErrOpenState immediately
// without executing the function, so the success is NOT recorded. This means
// RecordSuccess() silently no-ops when the circuit is open. For accurate outcome
// tracking, use Execute() instead.
func (cb *CircuitBreaker) RecordSuccess() {
	_, _ = cb.cb.Execute(func() (any, error) {
		return nil, nil
	})
}

// RecordFailure records a failed request
// For backward compatibility - wraps Execute with a failed operation
//
// LIMITATION: When the circuit is Open, Execute() returns ErrOpenState immediately
// without executing the function, so the failure is NOT recorded. This means
// RecordFailure() silently no-ops when the circuit is open. For accurate outcome
// tracking, use Execute() instead.
func (cb *CircuitBreaker) RecordFailure() {
	_, _ = cb.cb.Execute(func() (any, error) {
		return nil, fmt.Errorf("recorded failure")
	})
}

// Execute wraps an operation with circuit breaker protection
// This is the preferred way to use the circuit breaker.
//
// BEHAVIOR:
//   - Closed state: executes fn, records success/failure
//   - Open state: returns ErrOpenState immediately, does NOT execute fn
//   - HalfOpen state: executes fn up to MaxRequests times, then rejects
//
// STATE TRANSITIONS:
//   - Triggers open→half-open transition when Timeout expires
//   - Triggers half-open→closed on successful probe
//   - Triggers half-open→open on failed probe
//   - Evaluates ReadyToTrip after each failure in closed state
//
// Use Execute() for all actual operations. It handles state checking,
// outcome recording, and state transitions atomically.
func (cb *CircuitBreaker) Execute(fn func() (any, error)) (any, error) {
	return cb.cb.Execute(fn)
}

// IsOpen returns whether the circuit breaker is in open state
func (cb *CircuitBreaker) IsOpen() bool {
	return cb.cb.State() == gobreaker.StateOpen
}

// State returns the current state of the circuit breaker
func (cb *CircuitBreaker) State() gobreaker.State {
	return cb.cb.State()
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
