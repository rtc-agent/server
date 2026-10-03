// Package circuitbreaker provides Prometheus metrics for circuit breaker state changes.
package circuitbreaker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// circuitBreakerMetrics holds Prometheus metrics for circuit breaker events.
// All collectors are registered automatically via promauto and are safe for
// concurrent use.
type circuitBreakerMetrics struct {
	stateChangesTotal *prometheus.CounterVec // labels: name, from_state, to_state
	currentState      *prometheus.GaugeVec   // labels: name, state
}

// global circuitBreakerMetrics instance (promauto handles registration).
var globalCBMetrics = newCircuitBreakerMetrics()

// newCircuitBreakerMetrics creates and registers all circuit breaker metrics.
func newCircuitBreakerMetrics() *circuitBreakerMetrics {
	return &circuitBreakerMetrics{
		stateChangesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "circuitbreaker",
			Name:      "state_changes_total",
			Help:      "Total number of circuit breaker state changes.",
		}, []string{"name", "from_state", "to_state"}),

		currentState: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "rtc",
			Subsystem: "circuitbreaker",
			Name:      "state",
			Help:      "Current state of the circuit breaker (1=active, 0=inactive).",
		}, []string{"name", "state"}), // state: "closed", "open", "half_open"
	}
}

// recordStateChange records a state transition event.
func (m *circuitBreakerMetrics) recordStateChange(name string, from, to CircuitState) {
	fromStr := stateString(from)
	toStr := stateString(to)

	m.stateChangesTotal.WithLabelValues(name, fromStr, toStr).Inc()

	// Reset all states for this breaker, then set the new one
	for _, s := range []string{"closed", "open", "half_open"} {
		m.currentState.WithLabelValues(name, s).Set(0)
	}
	m.currentState.WithLabelValues(name, toStr).Set(1)
}

// stateString converts a CircuitState to its string representation.
func stateString(s CircuitState) string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// recordMetricsForEvent is a helper to record metrics from a CircuitBreakerEvent.
func recordMetricsForEvent(event CircuitBreakerEvent) {
	globalCBMetrics.recordStateChange(event.ProviderName, event.OldState, event.NewState)
}
