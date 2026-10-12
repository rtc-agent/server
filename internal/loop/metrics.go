package loop

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// loopMetrics holds all Prometheus metrics for the loop subsystem.
// All collectors are registered automatically via promauto and are safe for
// concurrent use.
var loopMetrics = struct {
	// tasksTotal counts the number of loop tasks processed.
	// Labels: status (success|failure)
	tasksTotal *prometheus.CounterVec

	// taskDuration tracks the duration of loop task processing in seconds.
	// Labels: outcome (success|failure)
	taskDuration *prometheus.HistogramVec

	// recoveryScansTotal counts the number of recovery scans performed.
	recoveryScansTotal prometheus.Counter

	// recoveryAttemptsTotal counts recovery attempts by outcome.
	// Labels: status (success|failure|exhausted)
	recoveryAttemptsTotal *prometheus.CounterVec
}{
	tasksTotal: promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rtc",
		Subsystem: "loop_worker",
		Name:      "tasks_total",
		Help:      "Total number of loop tasks processed by the worker.",
	}, []string{"status"}), // status: "success", "failure"

	taskDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rtc",
		Subsystem: "loop_worker",
		Name:      "task_duration_seconds",
		Help:      "Duration of loop task processing in seconds.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms ~ 5s
	}, []string{"outcome"}), // outcome: "success", "failure"

	recoveryScansTotal: promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "rtc",
		Subsystem: "loop_recovery",
		Name:      "scans_total",
		Help:      "Total number of loop recovery scans performed.",
	}),

	recoveryAttemptsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "rtc",
		Subsystem: "loop_recovery",
		Name:      "attempts_total",
		Help:      "Total number of loop recovery attempts.",
	}, []string{"status"}), // status: "success", "failure", "exhausted"
}

// recordTaskSuccess records a successful task processing.
func recordTaskSuccess(duration time.Duration) {
	loopMetrics.tasksTotal.WithLabelValues("success").Inc()
	loopMetrics.taskDuration.WithLabelValues("success").Observe(duration.Seconds())
}

// recordTaskFailure records a failed task processing.
func recordTaskFailure(duration time.Duration) {
	loopMetrics.tasksTotal.WithLabelValues("failure").Inc()
	loopMetrics.taskDuration.WithLabelValues("failure").Observe(duration.Seconds())
}

// recordRecoveryScan records a recovery scan.
func recordRecoveryScan() {
	loopMetrics.recoveryScansTotal.Inc()
}

// recordRecoverySuccess records a successful recovery attempt.
func recordRecoverySuccess() {
	loopMetrics.recoveryAttemptsTotal.WithLabelValues("success").Inc()
}

// recordRecoveryFailure records a failed recovery attempt.
func recordRecoveryFailure() {
	loopMetrics.recoveryAttemptsTotal.WithLabelValues("failure").Inc()
}

// recordRecoveryExhausted records a loop marked as exhausted.
func recordRecoveryExhausted() {
	loopMetrics.recoveryAttemptsTotal.WithLabelValues("exhausted").Inc()
}
