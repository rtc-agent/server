package turnagent

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoActiveTurn is returned by LookupTurn when no active turn is found
// for the session. This typically means the turn was cancelled, completed,
// or failed between the resume work item being published and processed.
var ErrNoActiveTurn = errors.New("no active turn for session")

// errMissing returns a validation error for a missing required Config field.
func errMissing(field string) error {
	return fmt.Errorf("config field %q is required", field)
}

// IsPromptTooLongError checks if the error is a prompt-too-long error from
// any supported LLM provider (Claude, OpenAI, etc.).
//
// Detected patterns (case-insensitive):
//   - "prompt is too long"        — Anthropic Claude
//   - "reduce your prompt"        — Anthropic Claude
//   - "context_length_exceeded"   — OpenAI
//   - "maximum context length"    — OpenAI
func IsPromptTooLongError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	for _, pattern := range promptTooLongPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// promptTooLongPatterns lists the case-insensitive substrings that indicate
// a prompt-too-long error from an LLM provider.
var promptTooLongPatterns = []string{
	"prompt is too long",      // Claude
	"reduce your prompt",      // Claude
	"context_length_exceeded", // OpenAI
	"maximum context length",  // OpenAI
}

// StreamIdleTimeoutError indicates a stream read timeout (consumeStream 3 minutes without data).
type StreamIdleTimeoutError struct {
	SessionID string
	TurnID    string
	Timeout   time.Duration
	AgentName string // agent producing the stream (aids diagnosis)
	Role      string // message role: "assistant" or "tool"
	ToolName  string // tool name (empty for assistant messages)
}

func (e *StreamIdleTimeoutError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stream idle timeout after %v (session=%s, turn=%s", e.Timeout, e.SessionID, e.TurnID)
	if e.AgentName != "" {
		fmt.Fprintf(&b, ", agent=%s", e.AgentName)
	}
	if e.Role != "" {
		fmt.Fprintf(&b, ", role=%s", e.Role)
	}
	if e.ToolName != "" {
		fmt.Fprintf(&b, ", tool=%s", e.ToolName)
	}
	b.WriteString(")")
	return b.String()
}

// IsStreamIdleTimeout checks whether the error is a stream idle timeout.
func IsStreamIdleTimeout(err error) bool {
	var target *StreamIdleTimeoutError
	return errors.As(err, &target)
}
