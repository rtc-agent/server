package turnagent

import (
	"strings"
	"testing"
)

func TestFormatSystemReminder(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{
			name:     "simple content",
			content:  "This is a system reminder",
			expected: "<system-reminder>\nThis is a system reminder\n</system-reminder>",
		},
		{
			name:     "multiline content",
			content:  "Line 1\nLine 2\nLine 3",
			expected: "<system-reminder>\nLine 1\nLine 2\nLine 3\n</system-reminder>",
		},
		{
			name:     "empty content",
			content:  "",
			expected: "<system-reminder>\n\n</system-reminder>",
		},
		{
			name:     "content with special characters",
			content:  "Task completed with <special> & \"characters\"",
			expected: "<system-reminder>\nTask completed with <special> & \"characters\"\n</system-reminder>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatSystemReminder(tt.content)
			if result != tt.expected {
				t.Errorf("FormatSystemReminder() = %q, want %q", result, tt.expected)
			}

			// Verify the result contains the XML tags
			if !strings.HasPrefix(result, "<system-reminder>\n") {
				t.Errorf("Result should start with '<system-reminder>\\n', got %q", result)
			}
			if !strings.HasSuffix(result, "\n</system-reminder>") {
				t.Errorf("Result should end with '\\n</system-reminder>', got %q", result)
			}

			// Verify the content is preserved
			if !strings.Contains(result, tt.content) {
				t.Errorf("Result should contain the original content %q", tt.content)
			}
		})
	}
}

func TestFormatSystemReminder_Usage(t *testing.T) {
	// Test that the function is meant to be used as user-role message content
	// This is a convention test, not a functional test
	content := "Sub-agent completed task"
	formatted := FormatSystemReminder(content)

	// The formatted string should be suitable for use as user-role message content
	// and should clearly indicate it's a system-level instruction
	if !strings.Contains(formatted, "<system-reminder>") {
		t.Error("Formatted content should contain <system-reminder> tag")
	}

	// The tags should be properly closed
	if strings.Count(formatted, "<system-reminder>") != strings.Count(formatted, "</system-reminder>") {
		t.Error("Opening and closing tags should match")
	}
}
