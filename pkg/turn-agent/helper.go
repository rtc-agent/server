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

// logIfEnabled calls the logger if it is not nil.
func (a *Agent) logIfEnabled(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
	if a.cfg.Logger != nil {
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
}

// recordMetricIfEnabled calls the metrics recorder if it is not nil.
func (a *Agent) recordMetricIfEnabled(ctx context.Context, record func(Metrics)) {
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
