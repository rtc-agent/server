package turnagent

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestExtractTraceFromCtx(t *testing.T) {
	tests := []struct {
		name        string
		ctx         context.Context
		wantTraceID string
		wantSpanID  string
	}{
		{
			name:        "nil context",
			ctx:         nil,
			wantTraceID: "",
			wantSpanID:  "",
		},
		{
			name:        "empty context",
			ctx:         context.Background(),
			wantTraceID: "",
			wantSpanID:  "",
		},
		{
			name: "valid span context",
			ctx: func() context.Context {
				traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
				spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
				sc := trace.NewSpanContext(trace.SpanContextConfig{
					TraceID:    traceID,
					SpanID:     spanID,
					TraceFlags: trace.FlagsSampled,
				})
				return trace.ContextWithSpanContext(context.Background(), sc)
			}(),
			wantTraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			wantSpanID:  "00f067aa0ba902b7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTraceID, gotSpanID := ExtractTraceFromCtx(tt.ctx)
			if gotTraceID != tt.wantTraceID {
				t.Errorf("ExtractTraceFromCtx() traceID = %v, want %v", gotTraceID, tt.wantTraceID)
			}
			if gotSpanID != tt.wantSpanID {
				t.Errorf("ExtractTraceFromCtx() spanID = %v, want %v", gotSpanID, tt.wantSpanID)
			}
		})
	}
}

func TestWithTraceContext(t *testing.T) {
	tests := []struct {
		name        string
		traceID     string
		spanID      string
		wantTraceID string
		wantSpanID  string
	}{
		{
			name:        "empty trace_id",
			traceID:     "",
			spanID:      "00f067aa0ba902b7",
			wantTraceID: "",
			wantSpanID:  "",
		},
		{
			name:        "empty span_id",
			traceID:     "4bf92f3577b34da6a3ce929d0e0e4736",
			spanID:      "",
			wantTraceID: "",
			wantSpanID:  "",
		},
		{
			name:        "invalid trace_id",
			traceID:     "invalid",
			spanID:      "00f067aa0ba902b7",
			wantTraceID: "",
			wantSpanID:  "",
		},
		{
			name:        "valid trace context",
			traceID:     "4bf92f3577b34da6a3ce929d0e0e4736",
			spanID:      "00f067aa0ba902b7",
			wantTraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			wantSpanID:  "00f067aa0ba902b7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := WithTraceContext(context.Background(), tt.traceID, tt.spanID)
			gotTraceID, gotSpanID := ExtractTraceFromCtx(ctx)
			if gotTraceID != tt.wantTraceID {
				t.Errorf("WithTraceContext() traceID = %v, want %v", gotTraceID, tt.wantTraceID)
			}
			if gotSpanID != tt.wantSpanID {
				t.Errorf("WithTraceContext() spanID = %v, want %v", gotSpanID, tt.wantSpanID)
			}

			// Verify that the restored span context is marked as Remote
			if tt.wantTraceID != "" {
				span := trace.SpanFromContext(ctx)
				if !span.SpanContext().IsRemote() {
					t.Errorf("WithTraceContext() span context should be marked as Remote")
				}
			}
		})
	}
}

func TestTraceContextRoundtrip(t *testing.T) {
	// Create a valid span context
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	originalCtx := trace.ContextWithSpanContext(context.Background(), sc)

	// Extract trace context
	extractedTraceID, extractedSpanID := ExtractTraceFromCtx(originalCtx)

	// Restore trace context
	restoredCtx := WithTraceContext(context.Background(), extractedTraceID, extractedSpanID)

	// Verify roundtrip
	restoredTraceID, restoredSpanID := ExtractTraceFromCtx(restoredCtx)
	if restoredTraceID != extractedTraceID {
		t.Errorf("Roundtrip failed: traceID mismatch: got %v, want %v", restoredTraceID, extractedTraceID)
	}
	if restoredSpanID != extractedSpanID {
		t.Errorf("Roundtrip failed: spanID mismatch: got %v, want %v", restoredSpanID, extractedSpanID)
	}
}
