package turnagent

import "context"

// Metrics is the interface for collecting metrics within turn-agent.
// Implementations should be thread-safe as Metrics may be called from multiple goroutines.
//
// Each method corresponds to a specific event type and receives a struct containing
// all relevant attributes for that event. This design provides type safety and
// IDE support compared to generic Record(name, value, labels) patterns.
//
// Example implementation using Prometheus:
//
//	type PrometheusMetrics struct {
//	    turnDuration   *prometheus.HistogramVec
//	    turnStatus     *prometheus.CounterVec
//	    llmTokens      *prometheus.CounterVec
//	    llmLatency     *prometheus.HistogramVec
//	    interruptCount *prometheus.CounterVec
//	}
//
//	func (m *PrometheusMetrics) RecordTurn(ctx context.Context, attrs TurnMetricsAttrs) {
//	    m.turnDuration.WithLabelValues(attrs.TurnID).Observe(float64(attrs.DurationMs))
//	    m.turnStatus.WithLabelValues(attrs.Status).Inc()
//	}
type Metrics interface {
	// RecordTurn records metrics for a completed turn.
	RecordTurn(ctx context.Context, attrs TurnMetricsAttrs)

	// RecordLLMCall records metrics for an LLM API call.
	//
	// NOTE: This method is NOT called automatically by the Agent. Consumers
	// have two options for LLM-call-level observability:
	//
	//  1. Automatic (recommended): configure Config.Callbacks with an eino
	//     callback handler (e.g., eino-ext/callbacks/cozeloop,
	//     eino-ext/callbacks/langfuse). The Agent injects these handlers
	//     into each turn's context, and eino's ChatModel/Tool implementations
	//     invoke them automatically on every call.
	//
	//  2. Manual: invoke RecordLLMCall yourself, for example from
	//     PublishEvent when you observe an LLM output event (token counts
	//     are available on msg.ResponseMeta.Usage), or from a custom
	//     ChatModel wrapper.
	//
	// If neither option is used, this method will never fire — implementations
	// should treat zero invocations as normal.
	RecordLLMCall(ctx context.Context, attrs LLMCallMetricsAttrs)

	// RecordInterrupt records an interrupt event.
	RecordInterrupt(ctx context.Context, attrs InterruptMetricsAttrs)

	// RecordCheckpoint records a checkpoint save/load operation.
	//
	// NOTE: turn-agent does not call this directly — the eino TurnLoop owns
	// checkpoint persistence and does not surface per-operation telemetry to
	// the enclosing code. Applications that wrap their adk.CheckPointStore
	// with an instrumented decorator should call RecordCheckpoint from that
	// decorator. Implementations should treat zero invocations as normal.
	RecordCheckpoint(ctx context.Context, attrs CheckpointMetricsAttrs)
}

// TurnMetricsAttrs contains attributes for a turn event.
type TurnMetricsAttrs struct {
	// SessionID is the session this turn belongs to.
	SessionID string
	// TurnID is the unique identifier for this turn.
	TurnID string
	// WorkKind is the kind of work that triggered this turn: "submit" or "resume".
	WorkKind string
	// Status is the turn outcome: "success", "interrupt", "fail", or "cancel".
	Status string
	// DurationMs is the turn duration in milliseconds (from BeginTurn/ResumeTurn
	// to the terminal callback).
	DurationMs int64
	// Error is set if the turn failed (nil for success/interrupt/cancel).
	Error error
}

// LLMCallMetricsAttrs contains attributes for an LLM API call.
//
// The Agent does not invoke RecordLLMCall directly. See the Metrics interface
// docs for the recommended ways to collect this metric.
type LLMCallMetricsAttrs struct {
	// SessionID is the session this call belongs to.
	SessionID string
	// TurnID is the turn this call belongs to.
	TurnID string
	// Model is the model identifier (e.g., "gpt-4", "claude-3-opus").
	Model string
	// InputTokens is the number of tokens in the prompt.
	InputTokens int
	// OutputTokens is the number of tokens in the completion.
	OutputTokens int
	// TotalTokens is the total tokens used (InputTokens + OutputTokens).
	TotalTokens int
	// LatencyMs is the API call latency in milliseconds.
	LatencyMs int64
	// Error is set if the call failed (nil for success).
	Error error
}

// InterruptMetricsAttrs contains attributes for an interrupt event.
type InterruptMetricsAttrs struct {
	// SessionID is the session this interrupt occurred in.
	SessionID string
	// TurnID is the turn where the interrupt occurred.
	TurnID string
	// InterruptID is the eino-assigned ID of the root interrupt context.
	InterruptID string
	// Reason is the interrupt reason. Mirrors the interruptInfo's concrete
	// type as a string for label use; applications may cast interruptInfo
	// in their own InterruptTurn callback for richer semantics.
	Reason string
}

// CheckpointMetricsAttrs contains attributes for a checkpoint operation.
//
// The Agent does not invoke RecordCheckpoint directly. Applications that
// want this metric should wrap their adk.CheckPointStore with an instrumented
// decorator that calls RecordCheckpoint on save/load.
type CheckpointMetricsAttrs struct {
	// SessionID is the session this checkpoint belongs to.
	SessionID string
	// Operation is "save" or "load".
	Operation string
	// DataSize is the checkpoint data size in bytes.
	DataSize int
	// DurationMs is the operation duration in milliseconds.
	DurationMs int64
	// Error is set if the operation failed (nil for success).
	Error error
}
