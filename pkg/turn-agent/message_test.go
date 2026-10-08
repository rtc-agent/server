package turnagent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestToEinoMessage(t *testing.T) {
	tests := []struct {
		name                string
		input               *Message
		wantNil             bool
		wantContent         string
		wantMultiContentLen int
		wantFirstTextPart   bool // true if first part of UserInputMultiContent should be text
	}{
		{
			name:    "nil message",
			input:   nil,
			wantNil: true,
		},
		{
			name: "message with Content only (no MultiContent)",
			input: &Message{
				Role:    RoleUser,
				Content: "hello world",
			},
			wantContent:         "hello world",
			wantMultiContentLen: 0, // UserInputMultiContent should be empty
		},
		{
			name: "message with MultiContent only (no text)",
			input: &Message{
				Role:    RoleUser,
				Content: "",
				MultiContent: []schema.MessageInputPart{
					{Type: schema.ChatMessagePartTypeImageURL},
				},
			},
			wantContent:         "",
			wantMultiContentLen: 1,
			wantFirstTextPart:   false, // no text part since Content is empty
		},
		{
			name: "message with Content and MultiContent",
			input: &Message{
				Role:    RoleUser,
				Content: "look at this image",
				MultiContent: []schema.MessageInputPart{
					{Type: schema.ChatMessagePartTypeImageURL},
				},
			},
			wantContent:         "look at this image", // Content preserved for backward compatibility
			wantMultiContentLen: 2,                    // Text part + Image part
			wantFirstTextPart:   true,                 // first part should be text
		},
		{
			name: "message with empty Content and empty MultiContent",
			input: &Message{
				Role:         RoleUser,
				Content:      "",
				MultiContent: []schema.MessageInputPart{},
			},
			wantContent:         "",
			wantMultiContentLen: 0, // empty MultiContent should not trigger UserInputMultiContent
		},
		{
			name: "message with multiple images in MultiContent",
			input: &Message{
				Role:    RoleUser,
				Content: "compare these images",
				MultiContent: []schema.MessageInputPart{
					{Type: schema.ChatMessagePartTypeImageURL},
					{Type: schema.ChatMessagePartTypeImageURL},
					{Type: schema.ChatMessagePartTypeImageURL},
				},
			},
			wantContent:         "compare these images",
			wantMultiContentLen: 4, // Text part + 3 Image parts
			wantFirstTextPart:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toEinoMessage(tt.input)

			if tt.wantNil {
				if result != nil {
					t.Errorf("toEinoMessage() = %v, want nil", result)
				}
				return
			}

			if result == nil {
				t.Fatalf("toEinoMessage() = nil, want non-nil")
			}

			// Check Content
			if result.Content != tt.wantContent {
				t.Errorf("result.Content = %q, want %q", result.Content, tt.wantContent)
			}

			// Check UserInputMultiContent length
			if len(result.UserInputMultiContent) != tt.wantMultiContentLen {
				t.Errorf("len(result.UserInputMultiContent) = %d, want %d",
					len(result.UserInputMultiContent), tt.wantMultiContentLen)
			}

			// Check first part is text when expected
			if tt.wantMultiContentLen > 0 && tt.wantFirstTextPart {
				firstPart := result.UserInputMultiContent[0]
				if firstPart.Type != schema.ChatMessagePartTypeText {
					t.Errorf("first part type = %v, want %v",
						firstPart.Type, schema.ChatMessagePartTypeText)
				}
				if firstPart.Text != tt.wantContent {
					t.Errorf("first part text = %q, want %q", firstPart.Text, tt.wantContent)
				}
			}
		})
	}
}

func TestToEinoMessage_ContentMergePrevention(t *testing.T) {
	// This test verifies the critical behavior: when both Content and MultiContent
	// are present, Content text must be merged into UserInputMultiContent as the first
	// Text part. This prevents the Claude adapter from silently dropping Content
	// (eino-ext Claude adapter uses else-if logic: UserInputMultiContent takes precedence).
	msg := &Message{
		Role:    RoleUser,
		Content: "user text content",
		MultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL},
		},
	}

	result := toEinoMessage(msg)

	// UserInputMultiContent should contain: [Text part, Image part]
	if len(result.UserInputMultiContent) != 2 {
		t.Fatalf("expected 2 parts in UserInputMultiContent, got %d", len(result.UserInputMultiContent))
	}

	// First part must be the text content
	if result.UserInputMultiContent[0].Type != schema.ChatMessagePartTypeText {
		t.Errorf("first part type = %v, want %v",
			result.UserInputMultiContent[0].Type, schema.ChatMessagePartTypeText)
	}
	if result.UserInputMultiContent[0].Text != "user text content" {
		t.Errorf("first part text = %q, want %q",
			result.UserInputMultiContent[0].Text, "user text content")
	}

	// Second part must be the image
	if result.UserInputMultiContent[1].Type != schema.ChatMessagePartTypeImageURL {
		t.Errorf("second part type = %v, want %v",
			result.UserInputMultiContent[1].Type, schema.ChatMessagePartTypeImageURL)
	}

	// Content should be preserved (backward compatibility)
	if result.Content != "user text content" {
		t.Errorf("result.Content = %q, want %q", result.Content, "user text content")
	}
}

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
