package agent

import (
	"strings"
	"testing"
)

func TestFormatCompactSummary(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "extract summary from analysis+summary blocks",
			input: `<analysis>
This is the analysis content that should be stripped.
</analysis>

<summary>
1. Primary Request and Intent:
   The user asked to fix a bug.

2. Key Technical Concepts:
   - Go programming
   - Context management
</summary>`,
			expected: `1. Primary Request and Intent:
   The user asked to fix a bug.

2. Key Technical Concepts:
   - Go programming
   - Context management`,
		},
		{
			name: "strip analysis only, no summary tags",
			input: `<analysis>
Analysis content.
</analysis>

This is the raw summary without tags.`,
			expected: "This is the raw summary without tags.",
		},
		{
			name:     "no tags at all",
			input:    "Just plain text summary.",
			expected: "Just plain text summary.",
		},
		{
			name: "empty analysis",
			input: `<analysis></analysis>
<summary>Summary content here.</summary>`,
			expected: "Summary content here.",
		},
		{
			name: "multiline summary content",
			input: `<analysis>
Line 1
Line 2
Line 3
</analysis>

<summary>
## Section 1
Content 1

## Section 2
Content 2

### Subsection
More content
</summary>`,
			expected: `## Section 1
Content 1

## Section 2
Content 2

### Subsection
More content`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatCompactSummary(tt.input)
			// Normalize whitespace for comparison.
			result = strings.TrimSpace(result)
			expected := strings.TrimSpace(tt.expected)
			if result != expected {
				t.Errorf("formatCompactSummary() mismatch\n\ngot:\n%s\n\nwant:\n%s", result, expected)
			}
		})
	}
}

func TestGetCompactPrompt(t *testing.T) {
	tests := []struct {
		mode         CompactMode
		mustContain  []string
		mustNotContain []string
	}{
		{
			mode: CompactModeFull,
			mustContain: []string{
				"TEXT ONLY",
				"Do NOT call any tools",
				"detailed summary of the conversation so far",
				"<analysis>",
				"<summary>",
			},
		},
		{
			mode: CompactModePartial,
			mustContain: []string{
				"TEXT ONLY",
				"RECENT portion of the conversation",
				"earlier messages are being kept intact",
			},
		},
		{
			mode: CompactModePartialUpTo,
			mustContain: []string{
				"TEXT ONLY",
				"continuing session",
				"newer messages that build on this context",
			},
		},
	}

	for _, tt := range tests {
		t.Run(string(rune('0'+tt.mode)), func(t *testing.T) {
			prompt := getCompactPrompt(tt.mode)

			for _, must := range tt.mustContain {
				if !strings.Contains(prompt, must) {
					t.Errorf("prompt missing required content: %q", must)
				}
			}

			for _, mustNot := range tt.mustNotContain {
				if strings.Contains(prompt, mustNot) {
					t.Errorf("prompt contains forbidden content: %q", mustNot)
				}
			}
		})
	}
}

func TestFormatCompactUserMessage(t *testing.T) {
	summary := "This is the summary of the conversation."
	result := formatCompactUserMessage(summary)

	if !strings.Contains(result, summary) {
		t.Errorf("user message missing summary content")
	}

	if !strings.Contains(result, "continued from a previous conversation") {
		t.Errorf("user message missing continuation context")
	}

	if !strings.Contains(result, "Recent messages are preserved verbatim") {
		t.Errorf("user message missing recent messages note")
	}
}
