package logger

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestMaskUserID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty string", "", ""},
		{"single char", "a", "***"},
		{"three chars", "abc", "***"},
		{"six chars", "abcdef", "***"},
		{"seven chars", "abcdefg", "abc***efg"},
		{"uuid format", "550e8400-e29b-41d4-a716-446655440000", "550***000"},
		{"short id", "user1", "***"},
		{"chinese chars", "用户张三丰", "***"},
		{"chinese chars long", "用户名称张三丰", "用户名***张三丰"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskUserID(tt.input)
			if got != tt.expected {
				t.Errorf("MaskUserID(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMaskEmail(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty string", "", ""},
		{"normal email", "alice@example.com", "a***@example.com"},
		{"single char local", "a@example.com", "***@example.com"},
		{"no at sign", "invalid", "inv***lid"},
		{"long local part", "administrator@corp.io", "a***@corp.io"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskEmail(tt.input)
			if got != tt.expected {
				t.Errorf("MaskEmail(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestMaskingCore(t *testing.T) {
	// Create an observed logger for testing.
	core, logs := observer.New(zapcore.InfoLevel)
	masked := &maskingCore{Core: core}
	l := zap.New(masked)

	// Log with sensitive fields.
	l.Info("test message",
		zap.String("user_id", "550e8400-e29b-41d4-a716-446655440000"),
		zap.String("email", "alice@example.com"),
		zap.String("safe_field", "visible-value"),
		zap.Any("user_id_raw", "raw-id-12345678"),
	)

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}

	fields := entries[0].ContextMap()

	// user_id should be masked (UUID first 3 + *** + last 3).
	if got, ok := fields["user_id"].(string); !ok || got != "550***000" {
		t.Errorf("user_id not masked correctly: got %v", fields["user_id"])
	}

	// email should be masked.
	if got, ok := fields["email"].(string); !ok || got != "a***@example.com" {
		t.Errorf("email not masked correctly: got %v", fields["email"])
	}

	// safe_field should remain unchanged.
	if got, ok := fields["safe_field"].(string); !ok || got != "visible-value" {
		t.Errorf("safe_field unexpectedly modified: got %v", fields["safe_field"])
	}

	// user_id_raw should be masked.
	if got, ok := fields["user_id_raw"].(string); !ok || got != "raw***678" {
		t.Errorf("user_id_raw not masked correctly: got %v", fields["user_id_raw"])
	}
}

func TestMaskingCoreWith(t *testing.T) {
	// Verify that With() also masks fields added via logger context.
	core, logs := observer.New(zapcore.InfoLevel)
	masked := &maskingCore{Core: core}
	base := zap.New(masked)

	child := base.With(zap.String("user_id", "abcdefghijklmnop"))
	child.Info("child log")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}

	fields := entries[0].ContextMap()
	if got, ok := fields["user_id"].(string); !ok || got != "abc***nop" {
		t.Errorf("user_id in With() not masked correctly: got %v", fields["user_id"])
	}
}

func BenchmarkMaskUserID(b *testing.B) {
	ids := []string{
		"",
		"short",
		"550e8400-e29b-41d4-a716-446655440000",
	}
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			_ = MaskUserID(id)
		}
	}
}
