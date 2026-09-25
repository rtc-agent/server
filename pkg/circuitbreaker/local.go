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
			requests := counts.Requests
			if requests == 0 {
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
func (cb *CircuitBreaker) RecordSuccess() {
	_, _ = cb.cb.Execute(func() (any, error) {
		return nil, nil
	})
}

// RecordFailure records a failed request
// For backward compatibility - wraps Execute with a failed operation
func (cb *CircuitBreaker) RecordFailure() {
	_, _ = cb.cb.Execute(func() (any, error) {
		return nil, fmt.Errorf("recorded failure")
	})
}

// Execute wraps an operation with circuit breaker protection
// This is the preferred way to use gobreaker
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
