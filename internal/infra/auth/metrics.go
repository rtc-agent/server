// Package auth_metrics provides Prometheus metrics for authentication events.
package auth

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// authMetrics holds Prometheus metrics for authentication events.
// All collectors are registered automatically via promauto and are safe for
// concurrent use.
type authMetrics struct {
	failuresTotal *prometheus.CounterVec // labels: type, reason
}

// global authMetrics instance (promauto handles registration).
var globalAuthMetrics = newAuthMetrics()

// newAuthMetrics creates and registers all auth metrics.
func newAuthMetrics() *authMetrics {
	return &authMetrics{
		failuresTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "auth",
			Name:      "failures_total",
			Help:      "Total number of authentication failures.",
		}, []string{"type", "reason"}),
		// type: "jwt", "oauth", "centrifuge"
		// reason: "expired", "invalid", "missing", "signature", "claims"
	}
}

// RecordFailure records an authentication failure.
func RecordFailure(authType, reason string) {
	globalAuthMetrics.failuresTotal.WithLabelValues(authType, reason).Inc()
}
