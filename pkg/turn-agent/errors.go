package turnagent

import (
	"fmt"
	"strings"
)

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
	"prompt is too long",     // Claude
	"reduce your prompt",     // Claude
	"context_length_exceeded", // OpenAI
	"maximum context length", // OpenAI
}
