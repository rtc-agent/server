package turnagent

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// =============================================================================
// Observability helpers
// =============================================================================

// cachedNoopSpan is a package-level noop span used when Tracer is nil.
// Using a noop trace span (rather than trace.SpanFromContext(ctx)) prevents
// unconditional SetAttributes/End calls from polluting an unrelated parent
// span that might already exist in the context.
var cachedNoopSpan trace.Span

func init() {
	_, cachedNoopSpan = nooptrace.NewTracerProvider().Tracer("turnagent").Start(context.Background(), "noop")
}

// log dispatches a log message to the appropriate logger method based on level.
func (a *Agent) log(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
	switch level {
	case LogLevelDebug:
		a.cfg.Logger.Debug(ctx, msg, fields)
	case LogLevelInfo:
		a.cfg.Logger.Info(ctx, msg, fields)
	case LogLevelWarn:
		a.cfg.Logger.Warn(ctx, msg, fields)
	case LogLevelError:
		a.cfg.Logger.Error(ctx, msg, fields)
	}
}

// recordMetricIfEnabled calls the metrics recorder if it is not nil.
func (a *Agent) recordMetricIfEnabled(record func(Metrics)) {
	if a.cfg.Metrics != nil {
		record(a.cfg.Metrics)
	}
}

// startSpanIfEnabled starts a span if tracer is not nil.
// When tracing is disabled, returns a cached noop span so callers can invoke
// SetAttributes/End unconditionally without polluting an unrelated parent span.
func (a *Agent) startSpanIfEnabled(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if a.cfg.Tracer != nil {
		return a.cfg.Tracer.Start(ctx, name, opts...)
	}
	return ctx, cachedNoopSpan
}

// addEventIfEnabled adds an event to the current span if tracer is not nil.
func (a *Agent) addEventIfEnabled(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if a.cfg.Tracer != nil {
		span := trace.SpanFromContext(ctx)
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// =============================================================================
// SessionTurnManager observability helpers
// =============================================================================

// startSpanIfEnabled starts a span if tracer is not nil.
// When tracing is disabled, returns a cached noop span so callers can invoke
// SetAttributes/End unconditionally without polluting an unrelated parent span.
func (mgr *SessionTurnManager) startSpanIfEnabled(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if mgr.cfg.Tracer != nil {
		return mgr.cfg.Tracer.Start(ctx, name, opts...)
	}
	return ctx, cachedNoopSpan
}

// =============================================================================
// Trace context propagation helpers (for cross-process boundaries)
// =============================================================================

// ExtractTraceFromCtx extracts OpenTelemetry trace information from the context.
// Returns trace_id and span_id as strings (empty strings if no valid span exists).
// This is useful for serializing trace context when passing it across process boundaries
// (e.g., when enqueuing tasks to Redis queues).
func ExtractTraceFromCtx(ctx context.Context) (traceID, spanID string) {
	if ctx == nil {
		return "", ""
	}
	span := trace.SpanFromContext(ctx)
	if span == nil || !span.SpanContext().IsValid() {
		return "", ""
	}
	sc := span.SpanContext()
	return sc.TraceID().String(), sc.SpanID().String()
}

// WithTraceContext restores OpenTelemetry trace context from serialized trace_id and span_id.
// This is useful when deserializing trace context from cross-process boundaries (e.g., Redis queues).
// The restored span context is marked as Remote=true, so subsequent span creation will create
// child spans that inherit the same TraceID.
func WithTraceContext(ctx context.Context, traceID, spanID string) context.Context {
	if traceID == "" || spanID == "" {
		return ctx
	}

	traceIDParsed, err1 := trace.TraceIDFromHex(traceID)
	spanIDParsed, err2 := trace.SpanIDFromHex(spanID)
	if err1 != nil || err2 != nil {
		return ctx
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceIDParsed,
		SpanID:     spanIDParsed,
		TraceFlags: trace.FlagsSampled,
		Remote:     true, // Mark as remote span context
	})

	// Use ContextWithRemoteSpanContext to mark this as a remote span
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}
