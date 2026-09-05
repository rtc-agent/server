package turnagent

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PrometheusMetrics 基于 Prometheus 的 Metrics 实现。
// 所有计数器/直方图通过 promauto 自动注册到默认 registry。
type PrometheusMetrics struct {
	turnDuration   *prometheus.HistogramVec
	turnStatus     *prometheus.CounterVec
	llmTokens      *prometheus.CounterVec
	llmLatency     *prometheus.HistogramVec
	interruptCount *prometheus.CounterVec
	checkpointOps  *prometheus.CounterVec
	checkpointSize *prometheus.HistogramVec
}

// NewPrometheusMetrics 创建并注册所有 Prometheus 指标。
func NewPrometheusMetrics() *PrometheusMetrics {
	return &PrometheusMetrics{
		turnDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "turn",
			Name:      "duration_seconds",
			Help:      "Turn execution duration in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12), // 0.1s ~ 204.8s
		}, []string{"work_kind", "status"}),

		turnStatus: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "turn",
			Name:      "total",
			Help:      "Total number of turns executed.",
		}, []string{"work_kind", "status"}),

		llmTokens: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "llm",
			Name:      "tokens_total",
			Help:      "Total LLM tokens consumed.",
		}, []string{"model", "type"}), // type: "input" or "output"

		llmLatency: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "llm",
			Name:      "request_duration_seconds",
			Help:      "LLM API request duration in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5s ~ 256s
		}, []string{"model", "status"}),

		interruptCount: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "interrupt",
			Name:      "total",
			Help:      "Total number of interrupts.",
		}, []string{"reason"}),

		checkpointOps: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "checkpoint",
			Name:      "operations_total",
			Help:      "Total checkpoint operations.",
		}, []string{"operation", "status"}),

		checkpointSize: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "checkpoint",
			Name:      "data_bytes",
			Help:      "Checkpoint data size in bytes.",
			Buckets:   prometheus.ExponentialBuckets(1024, 2, 10), // 1KB ~ 512MB
		}, []string{"operation"}),
	}
}

// RecordTurn records metrics for a completed turn.
func (m *PrometheusMetrics) RecordTurn(ctx context.Context, attrs TurnMetricsAttrs) {
	status := attrs.Status
	if status == "" {
		status = "unknown"
	}

	m.turnDuration.WithLabelValues(attrs.WorkKind, status).Observe(float64(attrs.DurationMs) / 1000)
	m.turnStatus.WithLabelValues(attrs.WorkKind, status).Inc()
}

// RecordLLMCall records metrics for an LLM API call.
func (m *PrometheusMetrics) RecordLLMCall(ctx context.Context, attrs LLMCallMetricsAttrs) {
	status := "success"
	if attrs.Error != nil {
		status = "error"
	}

	m.llmTokens.WithLabelValues(attrs.Model, "input").Add(float64(attrs.InputTokens))
	m.llmTokens.WithLabelValues(attrs.Model, "output").Add(float64(attrs.OutputTokens))
	m.llmLatency.WithLabelValues(attrs.Model, status).Observe(float64(attrs.LatencyMs) / 1000)
}

// RecordInterrupt records an interrupt event.
func (m *PrometheusMetrics) RecordInterrupt(ctx context.Context, attrs InterruptMetricsAttrs) {
	reason := attrs.Reason
	if reason == "" {
		reason = "unknown"
	}
	m.interruptCount.WithLabelValues(reason).Inc()
}

// RecordCheckpoint records a checkpoint save/load operation.
func (m *PrometheusMetrics) RecordCheckpoint(ctx context.Context, attrs CheckpointMetricsAttrs) {
	status := "success"
	if attrs.Error != nil {
		status = "error"
	}

	m.checkpointOps.WithLabelValues(attrs.Operation, status).Inc()
	m.checkpointSize.WithLabelValues(attrs.Operation).Observe(float64(attrs.DataSize))
}
