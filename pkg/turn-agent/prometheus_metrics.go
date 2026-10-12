package turnagent

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PrometheusMetrics implements Metrics using Prometheus.
// All counters and histograms are automatically registered to the default registry via promauto.
type PrometheusMetrics struct {
	turnDuration   *prometheus.HistogramVec
	turnStatus     *prometheus.CounterVec
	llmTokens      *prometheus.CounterVec
	llmLatency     *prometheus.HistogramVec
	interruptCount *prometheus.CounterVec
	checkpointOps  *prometheus.CounterVec
	checkpointSize *prometheus.HistogramVec

	// Attachment metrics
	attachmentTokens    *prometheus.CounterVec
	attachmentDuration  *prometheus.HistogramVec
	attachmentOps       *prometheus.CounterVec
	attachmentTruncated *prometheus.CounterVec

	// HTTP-level LLM API metrics (captured at transport layer, 100% coverage)
	llmHTTPRequestCount    *prometheus.CounterVec
	llmHTTPRequestDuration *prometheus.HistogramVec
	llmHTTPResponseTokens  *prometheus.CounterVec

	// Script metrics
	scriptExecutions        *prometheus.CounterVec
	scriptExecutionDuration *prometheus.HistogramVec
	scriptResultSize        *prometheus.HistogramVec
	scriptCodeSize          *prometheus.HistogramVec

	// Recovery metrics (stale turn scanner)
	staleTurnsRecovered *prometheus.CounterVec // label: status (running/pending/interrupted)

	// Business event metrics
	sessionCreatedTotal *prometheus.CounterVec
	sessionClosedTotal  *prometheus.CounterVec
	messagesSentTotal   *prometheus.CounterVec

	// Background goroutine tracking metrics (for monitoring context.WithoutCancel usage)
	activeGoroutines *prometheus.GaugeVec

	// Config fallback metrics (tracks degradation to YAML defaults)
	configFallback *prometheus.CounterVec

	// Token estimation metrics (fine-grained observability for EWMA and delta tracking)
	tokenDeltaHistogram *prometheus.HistogramVec // histogram of per-turn token deltas
	ewmaGauge           *prometheus.GaugeVec     // gauge of current EWMA values per session
}

// NewPrometheusMetrics creates and registers all Prometheus metrics.
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

		// Attachment metrics
		attachmentTokens: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "attachment",
			Name:      "tokens_total",
			Help:      "Total attachment tokens injected into LLM context.",
		}, []string{"name"}), // name: "TodoList", "SessionMemory", "UserMemory"

		attachmentDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "attachment",
			Name:      "build_duration_seconds",
			Help:      "Attachment build duration in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms ~ 512ms
		}, []string{"name"}),

		attachmentOps: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "attachment",
			Name:      "operations_total",
			Help:      "Total attachment build operations.",
		}, []string{"name", "status"}), // status: "success", "failed", "truncated"

		attachmentTruncated: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "attachment",
			Name:      "budget_truncated_total",
			Help:      "Total times attachment was truncated due to budget limit.",
		}, []string{"name"}),

		// HTTP-level LLM API metrics (captured at transport layer, 100% coverage)
		llmHTTPRequestCount: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "llm",
			Name:      "http_requests_total",
			Help:      "Total HTTP requests to LLM API (transport layer, 100% coverage).",
		}, []string{"model", "status"}), // status: "success" or "error"

		llmHTTPRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "llm",
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request duration to LLM API in seconds (transport layer).",
			Buckets:   prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5s ~ 256s
		}, []string{"model", "status"}),

		llmHTTPResponseTokens: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "llm",
			Name:      "http_tokens_total",
			Help:      "Total tokens from LLM HTTP responses (transport layer, 100% coverage).",
		}, []string{"model", "type"}), // type: "input" or "output"

		scriptExecutions: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "script",
			Name:      "executions_total",
			Help:      "Total number of script tool executions.",
		}, []string{"action", "status"}),

		scriptExecutionDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "script",
			Name:      "execution_duration_seconds",
			Help:      "Script execution duration in seconds (frontend-reported).",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15),
		}, []string{"action"}),

		scriptResultSize: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "script",
			Name:      "result_size_bytes",
			Help:      "Script execution result size in bytes.",
			Buckets:   prometheus.ExponentialBuckets(64, 2, 18),
		}, []string{"action"}),

		scriptCodeSize: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "script",
			Name:      "code_size_bytes",
			Help:      "Script source code size in bytes.",
			Buckets:   prometheus.ExponentialBuckets(64, 2, 12),
		}, []string{"action"}),

		staleTurnsRecovered: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "recovery",
			Name:      "stale_turns_recovered_total",
			Help:      "Total number of stale turns recovered by the periodic scanner.",
		}, []string{"status"}), // status: "running", "pending", "interrupted"

		// Business event metrics
		sessionCreatedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "session",
			Name:      "created_total",
			Help:      "Total number of sessions created.",
		}, []string{}),

		sessionClosedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "session",
			Name:      "closed_total",
			Help:      "Total number of sessions closed.",
		}, []string{"reason"}), // reason: "normal", "timeout", "error"

		messagesSentTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "message",
			Name:      "sent_total",
			Help:      "Total number of messages sent.",
		}, []string{"type"}), // type: "user", "assistant", "system"

		// Background goroutine tracking for monitoring detached context usage.
		// Helps detect goroutine leaks from context.WithoutCancel operations.
		activeGoroutines: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "rtc",
			Subsystem: "goroutine",
			Name:      "active",
			Help:      "Number of active background goroutines (context.WithoutCancel).",
		}, []string{"type"}), // type: "session_memory_extraction", "stream_drain"

		// Config fallback tracking for monitoring degradation to YAML defaults.
		// Incremented when DB config lookup fails and falls back to YAML baseline.
		configFallback: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "rtc",
			Subsystem: "config",
			Name:      "fallback_total",
			Help:      "Total number of config reads that fell back to YAML defaults (DB unavailable or key not found).",
		}, []string{"key", "reason"}), // reason: "not_found", "db_error"

		// Token estimation metrics for fine-grained observability.
		// tokenDeltaHistogram tracks the distribution of per-turn token deltas,
		// helping identify anomalous turns with unusually large or small token usage.
		// Buckets range from 100 to 1M tokens to capture the full spectrum of LLM calls.
		tokenDeltaHistogram: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "rtc",
			Subsystem: "token",
			Name:      "delta_tokens",
			Help:      "Distribution of per-turn token deltas (total tokens consumed in a single LLM call).",
			Buckets:   prometheus.ExponentialBuckets(100, 2, 15), // 100 ~ 1,638,400 tokens
		}, []string{"model", "type"}), // type: "input", "output", "total"

		// ewmaGauge tracks the current EWMA (Exponentially Weighted Moving Average) value
		// per session, enabling monitoring of token growth rate predictions over time.
		// This helps identify sessions with anomalous growth patterns or EWMA convergence issues.
		ewmaGauge: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "rtc",
			Subsystem: "token",
			Name:      "ewma",
			Help:      "Current EWMA (Exponentially Weighted Moving Average) value per session, predicting next round's token increment.",
		}, []string{"session_id"}),
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
	if attrs.CachedTokens > 0 {
		m.llmTokens.WithLabelValues(attrs.Model, "cached").Add(float64(attrs.CachedTokens))
	}
	if attrs.ReasoningTokens > 0 {
		m.llmTokens.WithLabelValues(attrs.Model, "reasoning").Add(float64(attrs.ReasoningTokens))
	}
	m.llmLatency.WithLabelValues(attrs.Model, status).Observe(float64(attrs.LatencyMs) / 1000)
}

// RecordLLMHTTPRequest records HTTP-level metrics for an LLM API request.
// This is called from the HTTP transport layer, ensuring 100% coverage.
func (m *PrometheusMetrics) RecordLLMHTTPRequest(ctx context.Context, attrs LLMHTTPMetricsAttrs) {
	status := "success"
	if attrs.StatusCode >= 400 {
		status = "error"
	}

	m.llmHTTPRequestCount.WithLabelValues(attrs.Model, status).Inc()
	m.llmHTTPRequestDuration.WithLabelValues(attrs.Model, status).Observe(float64(attrs.DurationMs) / 1000)

	if attrs.InputTokens > 0 {
		m.llmHTTPResponseTokens.WithLabelValues(attrs.Model, "input").Add(float64(attrs.InputTokens))
	}
	if attrs.OutputTokens > 0 {
		m.llmHTTPResponseTokens.WithLabelValues(attrs.Model, "output").Add(float64(attrs.OutputTokens))
	}
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

// RecordAttachment records metrics for an attachment build and injection.
func (m *PrometheusMetrics) RecordAttachment(ctx context.Context, attrs AttachmentMetricsAttrs) {
	// Record token consumption
	if attrs.Tokens > 0 {
		m.attachmentTokens.WithLabelValues(attrs.Name).Add(float64(attrs.Tokens))
	}

	// Record build duration
	if attrs.DurationMs > 0 {
		m.attachmentDuration.WithLabelValues(attrs.Name).Observe(float64(attrs.DurationMs) / 1000)
	}

	// Record operation status
	status := attrs.Status
	if status == "" {
		status = "unknown"
	}
	m.attachmentOps.WithLabelValues(attrs.Name, status).Inc()

	// Record truncation events
	if status == "truncated" {
		m.attachmentTruncated.WithLabelValues(attrs.Name).Inc()
	}
}

// ScriptExecutionMetricsAttrs contains attributes for a script execution event.
type ScriptExecutionMetricsAttrs struct {
	Action     string
	Status     string
	DurationMs int64
	ResultSize int64
	CodeSize   int64
}

// RecordScriptExecution records metrics for a script execution event.
// Defined on concrete type, not the Metrics interface (see design doc rationale).
func (m *PrometheusMetrics) RecordScriptExecution(ctx context.Context, attrs ScriptExecutionMetricsAttrs) {
	m.scriptExecutions.WithLabelValues(attrs.Action, attrs.Status).Inc()
	if attrs.DurationMs > 0 {
		m.scriptExecutionDuration.WithLabelValues(attrs.Action).Observe(float64(attrs.DurationMs) / 1000)
	}
	if attrs.ResultSize > 0 {
		m.scriptResultSize.WithLabelValues(attrs.Action).Observe(float64(attrs.ResultSize))
	}
	if attrs.CodeSize > 0 {
		m.scriptCodeSize.WithLabelValues(attrs.Action).Observe(float64(attrs.CodeSize))
	}
}

// RecordStaleTurnRecovery records a stale turn recovery event.
func (m *PrometheusMetrics) RecordStaleTurnRecovery(ctx context.Context, attrs StaleTurnRecoveryAttrs) {
	status := attrs.Status
	if status == "" {
		status = "unknown"
	}
	m.staleTurnsRecovered.WithLabelValues(status).Inc()
}

// RecordSessionCreated records a session creation event.
func (m *PrometheusMetrics) RecordSessionCreated(ctx context.Context) {
	m.sessionCreatedTotal.WithLabelValues().Inc()
}

// RecordSessionClosed records a session close event.
func (m *PrometheusMetrics) RecordSessionClosed(ctx context.Context, reason string) {
	if reason == "" {
		reason = "unknown"
	}
	m.sessionClosedTotal.WithLabelValues(reason).Inc()
}

// RecordMessageSent records a message send event.
func (m *PrometheusMetrics) RecordMessageSent(ctx context.Context, msgType string) {
	if msgType == "" {
		msgType = "unknown"
	}
	m.messagesSentTotal.WithLabelValues(msgType).Inc()
}

// IncBackgroundGoroutine increments the active goroutine count for the given type.
// Called when a background goroutine is spawned using context.WithoutCancel.
func (m *PrometheusMetrics) IncBackgroundGoroutine(goroutineType string) {
	if goroutineType == "" {
		goroutineType = "unknown"
	}
	m.activeGoroutines.WithLabelValues(goroutineType).Inc()
}

// DecBackgroundGoroutine decrements the active goroutine count for the given type.
// Called when a background goroutine exits (typically deferred).
func (m *PrometheusMetrics) DecBackgroundGoroutine(goroutineType string) {
	if goroutineType == "" {
		goroutineType = "unknown"
	}
	m.activeGoroutines.WithLabelValues(goroutineType).Dec()
}

// RecordConfigFallback records a config fallback event (degradation to YAML defaults).
// Called when DB config lookup fails and the system falls back to YAML baseline values.
func (m *PrometheusMetrics) RecordConfigFallback(ctx context.Context, key, reason string) {
	if reason == "" {
		reason = "unknown"
	}
	m.configFallback.WithLabelValues(key, reason).Inc()
}

// RecordTokenDelta records a token usage delta event for fine-grained observability.
// This tracks the distribution of per-turn token consumption, helping identify
// anomalous turns with unusually large or small token usage.
//
// Parameters:
//   - model: the model identifier (e.g., "claude-3-opus")
//   - deltaType: the type of delta ("input", "output", "total")
//   - tokens: the number of tokens consumed in this turn
//
// Called from token_callback.go after each LLM call to build histogram distributions.
func (m *PrometheusMetrics) RecordTokenDelta(ctx context.Context, model, deltaType string, tokens int) {
	if model == "" {
		model = "unknown"
	}
	if deltaType == "" {
		deltaType = "unknown"
	}
	if tokens > 0 {
		m.tokenDeltaHistogram.WithLabelValues(model, deltaType).Observe(float64(tokens))
	}
}

// UpdateEWMA updates the EWMA gauge for a session.
// This tracks the current EWMA (Exponentially Weighted Moving Average) value per session,
// enabling monitoring of token growth rate predictions over time.
//
// Parameters:
//   - sessionID: the session identifier
//   - ewma: the current EWMA value (predicted next round's token increment)
//
// Called from token_callback.go after EWMA is computed/updated.
// Note: Gauge values persist until explicitly updated or the session is closed.
//
// ⚠️ CARDINALITY WARNING: This metric uses sessionID as a label, creating one time series
// per active session. For deployments with millions of concurrent sessions, this can cause
// high memory usage in Prometheus. Expected usage:
//   - Small/Medium deployments (<10K sessions): No issue
//   - Large deployments (10K-100K sessions): Monitor Prometheus memory
//   - Very large deployments (>100K sessions): Consider disabling or aggregating
//
// Best practice: Call ResetEWMA() when sessions close to clean up stale time series.
func (m *PrometheusMetrics) UpdateEWMA(ctx context.Context, sessionID string, ewma float64) {
	if sessionID == "" {
		return
	}
	if ewma > 0 {
		m.ewmaGauge.WithLabelValues(sessionID).Set(ewma)
	}
}

// ResetEWMA resets the EWMA gauge for a session (e.g., when session is closed).
// This prevents stale EWMA values from accumulating in the gauge.
//
// ⚠️ IMPORTANT: This method MUST be called when sessions close to prevent unbounded
// growth of Prometheus time series. Without ResetEWMA, each session that ever existed
// will have a permanent gauge entry, causing memory issues at scale.
//
// Call sites: session close handlers, session cleanup routines, session deletion.
func (m *PrometheusMetrics) ResetEWMA(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	m.ewmaGauge.DeleteLabelValues(sessionID)
}
