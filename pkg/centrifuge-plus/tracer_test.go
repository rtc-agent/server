package centrifugeplus

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ========== encodeTraceParent 测试 ==========

func TestEncodeTraceParent_ValidSpanContext(t *testing.T) {
	traceID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: 0x01,
	})

	result := encodeTraceParent(sc)
	expected := "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestEncodeTraceParent_InvalidSpanContext(t *testing.T) {
	sc := trace.SpanContext{} // zero value, invalid
	result := encodeTraceParent(sc)
	if result != "" {
		t.Errorf("expected empty string for invalid span context, got %s", result)
	}
}

func TestEncodeTraceParent_NoopSpanContext(t *testing.T) {
	// noop TracerProvider 的 span context 通常是 invalid
	tp := noop.NewTracerProvider()
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-op")
	defer span.End()

	result := encodeTraceParent(span.SpanContext())
	// noop span context 通常是 invalid，应返回空
	if result != "" {
		t.Logf("noop span context produced: %s", result)
	}
}

// ========== decodeTraceParent 测试 ==========

func TestDecodeTraceParent_Valid(t *testing.T) {
	tp := "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"
	sc, err := decodeTraceParent(tp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sc.IsValid() {
		t.Error("expected valid span context")
	}
	if sc.IsRemote() != true {
		t.Error("expected Remote=true")
	}

	traceID := sc.TraceID()
	expectedTraceID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if traceID != expectedTraceID {
		t.Errorf("traceID mismatch: got %x", traceID)
	}

	spanID := sc.SpanID()
	expectedSpanID := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if spanID != expectedSpanID {
		t.Errorf("spanID mismatch: got %x", spanID)
	}

	if sc.TraceFlags() != 0x01 {
		t.Errorf("expected trace flags 0x01, got 0x%02x", sc.TraceFlags())
	}
}

func TestDecodeTraceParent_InvalidFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"too few parts", "00-abc-def"},
		{"too many parts", "00-abc-def-01-extra"},
		{"wrong version", "01-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"},
		{"invalid trace id length", "00-0102030405060708-0102030405060708-01"},
		{"invalid span id length", "00-0102030405060708090a0b0c0d0e0f10-0102-01"},
		{"invalid trace id hex", "00-xxxxxxxxxxxxxxxxxxxxxxxxxxxx-0102030405060708-01"},
		{"invalid span id hex", "00-0102030405060708090a0b0c0d0e0f10-xxxxxxxx-01"},
		{"invalid flags hex", "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-xx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeTraceParent(tt.input)
			if err == nil {
				t.Errorf("expected error for input %q, got nil", tt.input)
			}
		})
	}
}

// ========== encode/decode 往返测试 ==========

func TestEncodeDecodeRoundTrip(t *testing.T) {
	traceID := [16]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c}
	spanID := [8]byte{0xca, 0xfe, 0xba, 0xbe, 0x01, 0x02, 0x03, 0x04}

	original := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: 0x01,
	})

	encoded := encodeTraceParent(original)
	decoded, err := decodeTraceParent(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if original.TraceID() != decoded.TraceID() {
		t.Errorf("traceID mismatch: %x vs %x", original.TraceID(), decoded.TraceID())
	}
	if original.SpanID() != decoded.SpanID() {
		t.Errorf("spanID mismatch: %x vs %x", original.SpanID(), decoded.SpanID())
	}
	if original.TraceFlags() != decoded.TraceFlags() {
		t.Errorf("traceFlags mismatch: %x vs %x", original.TraceFlags(), decoded.TraceFlags())
	}
}

// ========== extractTraceParentFromPayload 测试 ==========

func TestExtractTraceParentFromPayload_WithTraceParent(t *testing.T) {
	payload := `{"msg":"hello"}__tp:00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01`
	clean, tp := extractTraceParentFromPayload(payload)

	if clean != `{"msg":"hello"}` {
		t.Errorf("unexpected clean payload: %s", clean)
	}
	if tp != "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01" {
		t.Errorf("unexpected traceparent: %s", tp)
	}
}

func TestExtractTraceParentFromPayload_WithoutTraceParent(t *testing.T) {
	payload := `{"msg":"hello"}`
	clean, tp := extractTraceParentFromPayload(payload)

	if clean != payload {
		t.Errorf("expected payload unchanged, got %s", clean)
	}
	if tp != "" {
		t.Errorf("expected empty traceparent, got %s", tp)
	}
}

func TestExtractTraceParentFromPayload_EmptyPayload(t *testing.T) {
	clean, tp := extractTraceParentFromPayload("")
	if clean != "" {
		t.Errorf("expected empty clean, got %s", clean)
	}
	if tp != "" {
		t.Errorf("expected empty traceparent, got %s", tp)
	}
}

func TestExtractTraceParentFromPayload_MultipleTraceParents(t *testing.T) {
	// 只提取最后一个有效的 __tp:（符合W3C traceparent格式）
	tp1 := "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01"
	tp2 := "00-aabbccddeeff00112233445566778899-abcdef0123456789-02"
	payload := `data__tp:` + tp1 + `__tp:` + tp2
	clean, tp := extractTraceParentFromPayload(payload)
	expectedClean := `data__tp:` + tp1
	if clean != expectedClean {
		t.Errorf("unexpected clean: %s, expected: %s", clean, expectedClean)
	}
	if tp != tp2 {
		t.Errorf("unexpected tp: %s, expected: %s", tp, tp2)
	}
}

// ========== TracingConfig 测试 ==========

func TestTracingConfig_DisabledReturnsNoop(t *testing.T) {
	cfg := TracingConfig{Enabled: false}
	tracer := cfg.tracer()
	if tracer == nil {
		t.Fatal("expected non-nil tracer")
	}
	// 应该可以安全使用
	_, span := tracer.Start(context.Background(), "test")
	span.End()
}

func TestTracingConfig_NilProviderReturnsNoop(t *testing.T) {
	cfg := TracingConfig{Enabled: true, Provider: nil}
	tracer := cfg.tracer()
	if tracer == nil {
		t.Fatal("expected non-nil tracer")
	}
	_, span := tracer.Start(context.Background(), "test")
	span.End()
}

// ========== recordError 测试 ==========

func TestRecordError_WithError(_ *testing.T) {
	// 使用 noop tracer，确保不 panic
	tp := noop.NewTracerProvider()
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-op")

	// 不应 panic
	recordError(span, nil)
	recordError(span, fmt.Errorf("test error"))
	span.End()
}

func TestRecordError_NilError(_ *testing.T) {
	tp := noop.NewTracerProvider()
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-op")

	// nil error 不应 panic
	recordError(span, nil)
	span.End()
}

// ========== isTraceParentCandidate 测试 ==========

func TestIsTraceParentCandidate_Valid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{
			name:  "standard valid",
			input: "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
			valid: true,
		},
		{
			name:  "all zeros",
			input: "00-00000000000000000000000000000000-0000000000000000-00",
			valid: true,
		},
		{
			name:  "all fs",
			input: "00-ffffffffffffffffffffffffffffffff-ffffffffffffffff-ff",
			valid: true,
		},
		{
			name:  "uppercase hex",
			input: "00-0102030405060708090A0B0C0D0E0F10-0102030405060708-01",
			valid: true,
		},
		{
			name:  "empty string",
			input: "",
			valid: false,
		},
		{
			name:  "too short",
			input: "00-0102030405-01020304-01",
			valid: false,
		},
		{
			name:  "wrong version",
			input: "01-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
			valid: false,
		},
		{
			name:  "trace id too short",
			input: "00-0102030405060708-0102030405060708-01",
			valid: false,
		},
		{
			name:  "span id too short",
			input: "00-0102030405060708090a0b0c0d0e0f10-01020304-01",
			valid: false,
		},
		{
			name:  "non-hex chars",
			input: "00-0102030405060708090a0b0c0d0e0fgg-0102030405060708-01",
			valid: false,
		},
		{
			name:  "just a random string",
			input: "hello-world",
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTraceParentCandidate(tt.input)
			if got != tt.valid {
				t.Errorf("isTraceParentCandidate(%q) = %v, want %v", tt.input, got, tt.valid)
			}
		})
	}
}
