// Package rtcqueue metrics provides Prometheus metrics for the queue subsystem.
package rtcqueue

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// queueMetrics holds all Prometheus metrics for the queue subsystem.
// All collectors are registered automatically via promauto and are safe for
// concurrent use.
type queueMetrics struct {
	publishTotal    *prometheus.CounterVec   // labels: status (success|error)
	claimTotal      *prometheus.CounterVec   // labels: status (success|empty|error)
	completeTotal   *prometheus.CounterVec   // labels: status (success|error)
	cancelTotal     *prometheus.CounterVec   // labels: reason
	waitDuration    *prometheus.HistogramVec // labels: (none) — publish to claim
	processDuration *prometheus.HistogramVec // labels: (none) — claim to complete
}

// global queueMetrics instance (promauto handles registration).
var globalQueueMetrics = newQueueMetrics()

// newQueueMetrics creates and registers all queue metrics.
func newQueueMetrics() *queueMetrics {
	return &queueMetrics{
		publishTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "queue",
			Name:      "publish_total",
			Help:      "Total number of work items published to the queue.",
		}, []string{"status"}), // status: "success", "error"

		claimTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "queue",
			Name:      "claim_total",
			Help:      "Total number of work item claim attempts.",
		}, []string{"status"}), // status: "success", "empty", "error"

		completeTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "queue",
			Name:      "complete_total",
			Help:      "Total number of work items completed.",
		}, []string{"status"}), // status: "success", "error"

		cancelTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "queue",
			Name:      "cancel_total",
			Help:      "Total number of work items cancelled.",
		}, []string{"reason"}),

		waitDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "queue",
			Name:      "wait_duration_seconds",
			Help:      "Time work items spend waiting in queue (publish to claim).",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms ~ 40s
		}, nil),

		processDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "queue",
			Name:      "processing_duration_seconds",
			Help:      "Time work items spend being processed (claim to complete).",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms ~ 400s
		}, nil),
	}
}

// recordPublish records a publish operation.
func (m *queueMetrics) recordPublish(status string) {
	m.publishTotal.WithLabelValues(status).Inc()
}

// recordClaim records a claim operation.
func (m *queueMetrics) recordClaim(status string) {
	m.claimTotal.WithLabelValues(status).Inc()
}

// recordComplete records a complete operation.
func (m *queueMetrics) recordComplete(status string) {
	m.completeTotal.WithLabelValues(status).Inc()
}

// recordCancel records a cancel operation.
func (m *queueMetrics) recordCancel(reason string) {
	m.cancelTotal.WithLabelValues(reason).Inc()
}

// recordWaitDuration records the time a work item waited in queue.
func (m *queueMetrics) recordWaitDuration(d time.Duration) {
	m.waitDuration.WithLabelValues().Observe(d.Seconds())
}

// recordProcessDuration records the time a work item spent being processed.
func (m *queueMetrics) recordProcessDuration(d time.Duration) {
	m.processDuration.WithLabelValues().Observe(d.Seconds())
}
