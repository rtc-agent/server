package centrifugeplus

import (
	"encoding/hex"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracingConfig configures distributed tracing for centrifuge-plus.
// When disabled, all tracing operations use a no-op tracer with zero overhead.
type TracingConfig struct {
	// Enabled controls whether tracing is active.
	Enabled bool

	// Provider is the OpenTelemetry TracerProvider.
	// If nil and Enabled is true, a no-op fallback is used.
	Provider trace.TracerProvider
}

// resolveProvider returns an active TracerProvider or a no-op fallback.
func (c TracingConfig) resolveProvider() trace.TracerProvider {
	if !c.Enabled || c.Provider == nil {
		return noop.NewTracerProvider()
	}
	return c.Provider
}

func (c TracingConfig) tracer() trace.Tracer {
	return c.resolveProvider().Tracer("centrifuge-plus")
}

// Span attribute keys for centrifuge-plus tracing.
const (
	AttributeChannel         = attribute.Key("centrifugeplus.channel")
	AttributeChannelType     = attribute.Key("centrifugeplus.channel_type")
	AttributeOffset          = attribute.Key("centrifugeplus.offset")
	AttributeEpoch           = attribute.Key("centrifugeplus.epoch")
	AttributeTaskID          = attribute.Key("centrifugeplus.task_id")
	AttributeEnqueued        = attribute.Key("centrifugeplus.enqueued")
	AttributeFromCache       = attribute.Key("centrifugeplus.from_cache")
	AttributeQueue           = attribute.Key("centrifugeplus.queue")
	AttributeMessageType     = attribute.Key("centrifugeplus.message_type")
	AttributeStreamMinOffset = attribute.Key("centrifugeplus.stream_min_offset")
)

// recordError sets span status to Error and records the error.
func recordError(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	}
}

// encodeTraceParent encodes a SpanContext into W3C traceparent format (00-{trace_id}-{span_id}-{flags}).
// Returns empty string if the SpanContext is invalid.
func encodeTraceParent(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	tid := sc.TraceID()
	sid := sc.SpanID()
	return fmt.Sprintf("00-%032x-%016x-%02x",
		[16]byte(tid), [8]byte(sid), byte(sc.TraceFlags()))
}

// decodeTraceParent parses a W3C traceparent string into a SpanContext.
func decodeTraceParent(tp string) (trace.SpanContext, error) {
	parts := strings.SplitN(tp, "-", 4)
	if len(parts) != 4 || parts[0] != "00" {
		return trace.SpanContext{}, fmt.Errorf("invalid traceparent: %s", tp)
	}

	tid, err := hex.DecodeString(parts[1])
	if err != nil || len(tid) != 16 {
		return trace.SpanContext{}, fmt.Errorf("invalid trace id in traceparent: %s", parts[1])
	}

	sid, err := hex.DecodeString(parts[2])
	if err != nil || len(sid) != 8 {
		return trace.SpanContext{}, fmt.Errorf("invalid span id in traceparent: %s", parts[2])
	}

	flags, err := hex.DecodeString(parts[3])
	if err != nil || len(flags) != 1 {
		return trace.SpanContext{}, fmt.Errorf("invalid trace flags in traceparent: %s", parts[3])
	}

	var traceID trace.TraceID
	var spanID trace.SpanID
	copy(traceID[:], tid)
	copy(spanID[:], sid)

	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.TraceFlags(flags[0]),
		Remote:     true,
	}), nil
}

// isTraceParentCandidate checks if s looks like a valid W3C traceparent (00-{32hex}-{16hex}-{2hex}).
func isTraceParentCandidate(s string) bool {
	// Minimum length: 00-00000000000000000000000000000000-0000000000000000-00 = 55 chars
	if len(s) < 55 || len(s) > 59 {
		return false
	}
	// Check prefix
	if !strings.HasPrefix(s, "00-") {
		return false
	}
	parts := strings.SplitN(s, "-", 4)
	if len(parts) != 4 {
		return false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	// Quick hex check
	for _, p := range parts[1:] {
		for _, c := range p {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// extractTraceParentFromPayload strips the __tp:{traceparent} suffix from a PUB/SUB payload
// and returns the clean payload and the extracted trace parent string.
// If no valid traceparent suffix is found, returns the original payload unchanged.
func extractTraceParentFromPayload(payload string) (cleanPayload string, traceParent string) {
	idx := strings.LastIndex(payload, "__tp:")
	if idx < 0 {
		return payload, ""
	}
	candidate := payload[idx+5:]
	if !isTraceParentCandidate(candidate) {
		return payload, ""
	}
	return payload[:idx], candidate
}
