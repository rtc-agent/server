package turnagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// StreamIdleTimeoutError tests
// ---------------------------------------------------------------------------

func TestStreamIdleTimeoutError_Error(t *testing.T) {
	err := &StreamIdleTimeoutError{
		SessionID: "sess-abc",
		TurnID:    "turn-xyz",
		Timeout:   3 * time.Minute,
	}
	got := err.Error()
	// Should contain the timeout duration, session id, and turn id.
	for _, want := range []string{"3m0s", "sess-abc", "turn-xyz"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q; missing %q", got, want)
		}
	}
}

func TestIsStreamIdleTimeout_Detection(t *testing.T) {
	inner := &StreamIdleTimeoutError{
		SessionID: "s1",
		TurnID:    "t1",
		Timeout:   180 * time.Second,
	}
	// Direct reference.
	if !IsStreamIdleTimeout(inner) {
		t.Error("expected IsStreamIdleTimeout=true for direct reference")
	}
	// Wrapped via fmt.Errorf — errors.As should still penetrate.
	wrapped := fmt.Errorf("consume stream failed: %w", inner)
	if !IsStreamIdleTimeout(wrapped) {
		t.Error("expected IsStreamIdleTimeout=true for wrapped error")
	}
}

func TestIsStreamIdleTimeout_NonMatch(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("some error")},
		{"context canceled", errors.New("context canceled")},
		{"timeout string", errors.New("timeout after 3m0s")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if IsStreamIdleTimeout(tc.err) {
				t.Errorf("IsStreamIdleTimeout(%v) = true; want false", tc.err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsPromptTooLongError tests
// ---------------------------------------------------------------------------

func TestIsPromptTooLongError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"claude pattern", errors.New("prompt is too long; please reduce"), true},
		{"claude reduce pattern", errors.New("Please reduce your prompt"), true},
		{"openai context_length", errors.New("context_length_exceeded"), true},
		{"openai maximum context", errors.New("maximum context length is 128000"), true},
		{"case insensitive", errors.New("PROMPT IS TOO LONG"), true},
		{"unrelated error", errors.New("something else went wrong"), false},
		{"wrapped prompt error", fmt.Errorf("api call: %w", errors.New("prompt is too long")), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPromptTooLongError(tc.err)
			if got != tc.want {
				t.Errorf("IsPromptTooLongError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MarshalSubmitPayload tests
// ---------------------------------------------------------------------------

func TestMarshalSubmitPayload(t *testing.T) {
	sessionID := "550e8400-e29b-41d4-a716-446655440000"
	data := MarshalSubmitPayload(sessionID, 1)

	// Should be valid JSON.
	var payload WorkPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("MarshalSubmitPayload returned invalid JSON: %v", err)
	}

	if payload.Kind != WorkKindSubmit {
		t.Errorf("Kind = %q, want %q", payload.Kind, WorkKindSubmit)
	}
	if payload.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", payload.SessionID, sessionID)
	}
}

func TestMarshalSubmitPayload_EmptySessionID(t *testing.T) {
	data := MarshalSubmitPayload("", 0)
	var payload WorkPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload.Kind != WorkKindSubmit {
		t.Errorf("Kind = %q, want %q", payload.Kind, WorkKindSubmit)
	}
}
