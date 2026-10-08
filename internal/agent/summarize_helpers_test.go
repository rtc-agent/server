package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestFormatMessagesForCompact(t *testing.T) {
	t.Run("user message with Content only (original behavior)", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role:    schema.User,
				Content: "Hello, how are you?",
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "Hello, how are you?") {
			t.Errorf("expected content to contain 'Hello, how are you?', got %q", result)
		}
		if !strings.Contains(result, "## user (turn 1)") {
			t.Errorf("expected heading '## user (turn 1)', got %q", result)
		}
	})

	t.Run("user message with MultiContent only (no text)", func(t *testing.T) {
		base64Data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
		msgs := []*schema.Message{
			{
				Role: schema.User,
				UserInputMultiContent: []schema.MessageInputPart{
					{
						Type: schema.ChatMessagePartTypeImageURL,
						Image: &schema.MessageInputImage{
							MessagePartCommon: schema.MessagePartCommon{
								Base64Data: &base64Data,
								MIMEType:   "image/png",
							},
						},
					},
				},
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "[image]") {
			t.Errorf("expected [image] marker, got %q", result)
		}
		if strings.Contains(result, "iVBORw0KGgo") {
			t.Errorf("should not contain base64 data, got %q", result)
		}
	})

	t.Run("user message with Content and MultiContent (no duplication)", func(t *testing.T) {
		// When UserInputMultiContent is present, Content is already merged as the first Text part
		// by toEinoMessage. So we should only iterate UserInputMultiContent, not write Content separately.
		base64Data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
		msgs := []*schema.Message{
			{
				Role:    schema.User,
				Content: "Look at this image", // This is already in UserInputMultiContent as first Text part
				UserInputMultiContent: []schema.MessageInputPart{
					{
						Type: schema.ChatMessagePartTypeText,
						Text: "Look at this image", // Same as Content (merged by toEinoMessage)
					},
					{
						Type: schema.ChatMessagePartTypeImageURL,
						Image: &schema.MessageInputImage{
							MessagePartCommon: schema.MessagePartCommon{
								Base64Data: &base64Data,
								MIMEType:   "image/png",
							},
						},
					},
				},
			},
		}

		result := formatMessagesForCompact(msgs)

		// Text should appear only once, not twice
		textCount := strings.Count(result, "Look at this image")
		if textCount != 1 {
			t.Errorf("text appeared %d times, expected 1 (no duplication), got %q", textCount, result)
		}
		if !strings.Contains(result, "[image]") {
			t.Errorf("expected [image] marker, got %q", result)
		}
	})

	t.Run("user message with mixed Text and Image in MultiContent", func(t *testing.T) {
		base64Data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
		msgs := []*schema.Message{
			{
				Role: schema.User,
				UserInputMultiContent: []schema.MessageInputPart{
					{
						Type: schema.ChatMessagePartTypeText,
						Text: "First text part",
					},
					{
						Type: schema.ChatMessagePartTypeImageURL,
						Image: &schema.MessageInputImage{
							MessagePartCommon: schema.MessagePartCommon{
								Base64Data: &base64Data,
								MIMEType:   "image/jpeg",
							},
						},
					},
					{
						Type: schema.ChatMessagePartTypeText,
						Text: "Second text part",
					},
				},
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "First text part") {
			t.Errorf("expected 'First text part', got %q", result)
		}
		if !strings.Contains(result, "Second text part") {
			t.Errorf("expected 'Second text part', got %q", result)
		}
		if !strings.Contains(result, "[image]") {
			t.Errorf("expected [image] marker, got %q", result)
		}
	})

	t.Run("user message with Text and File in MultiContent", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role: schema.User,
				UserInputMultiContent: []schema.MessageInputPart{
					{
						Type: schema.ChatMessagePartTypeText,
						Text: "Check this document",
					},
					{
						Type: schema.ChatMessagePartTypeFileURL,
					},
				},
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "Check this document") {
			t.Errorf("expected 'Check this document', got %q", result)
		}
		if !strings.Contains(result, "[document]") {
			t.Errorf("expected [document] marker, got %q", result)
		}
	})

	t.Run("user message with empty Text part in MultiContent", func(t *testing.T) {
		base64Data := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
		msgs := []*schema.Message{
			{
				Role: schema.User,
				UserInputMultiContent: []schema.MessageInputPart{
					{
						Type: schema.ChatMessagePartTypeText,
						Text: "", // Empty text
					},
					{
						Type: schema.ChatMessagePartTypeImageURL,
						Image: &schema.MessageInputImage{
							MessagePartCommon: schema.MessagePartCommon{
								Base64Data: &base64Data,
								MIMEType:   "image/png",
							},
						},
					},
				},
			},
		}

		result := formatMessagesForCompact(msgs)

		// Empty text part should not output anything
		if !strings.Contains(result, "[image]") {
			t.Errorf("expected [image] marker, got %q", result)
		}
		// Should only have one heading (not duplicated by empty text)
		headingCount := strings.Count(result, "## user (turn")
		if headingCount != 1 {
			t.Errorf("expected 1 heading, got %d: %q", headingCount, result)
		}
	})

	t.Run("user message with Content and MultiContent both empty", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role: schema.User,
			},
		}

		result := formatMessagesForCompact(msgs)

		// Should not output heading or content for empty message
		if strings.Contains(result, "## user") {
			t.Errorf("should not output heading for empty message, got %q", result)
		}
	})

	t.Run("assistant message with tool calls", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role:    schema.Assistant,
				Content: "Let me search for that.",
				ToolCalls: []schema.ToolCall{
					{
						ID: "call_123",
						Function: schema.FunctionCall{
							Name:      "web_search",
							Arguments: `{"query": "test"}`,
						},
					},
				},
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "Let me search for that.") {
			t.Errorf("expected assistant content, got %q", result)
		}
		if !strings.Contains(result, "**web_search**") {
			t.Errorf("expected tool call name, got %q", result)
		}
		if !strings.Contains(result, "call_123") {
			t.Errorf("expected tool call ID, got %q", result)
		}
	})

	t.Run("tool result message", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role:       schema.Tool,
				Content:    "Search result: test data",
				ToolName:   "web_search",
				ToolCallID: "call_123",
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "Search result: test data") {
			t.Errorf("expected tool result content, got %q", result)
		}
		if !strings.Contains(result, "web_search") {
			t.Errorf("expected tool name, got %q", result)
		}
		if !strings.Contains(result, "call_123") {
			t.Errorf("expected tool call ID, got %q", result)
		}
	})

	t.Run("system message is skipped", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role:    schema.System,
				Content: "You are a helpful assistant.",
			},
			{
				Role:    schema.User,
				Content: "Hello",
			},
		}

		result := formatMessagesForCompact(msgs)

		if strings.Contains(result, "You are a helpful assistant") {
			t.Errorf("system message should be skipped, got %q", result)
		}
		if !strings.Contains(result, "Hello") {
			t.Errorf("user message should be included, got %q", result)
		}
	})

	t.Run("multiple messages in conversation", func(t *testing.T) {
		msgs := []*schema.Message{
			{
				Role:    schema.User,
				Content: "First message",
			},
			{
				Role:    schema.Assistant,
				Content: "First response",
			},
			{
				Role:    schema.User,
				Content: "Second message",
			},
		}

		result := formatMessagesForCompact(msgs)

		if !strings.Contains(result, "## user (turn 1)") {
			t.Errorf("expected first user turn, got %q", result)
		}
		if !strings.Contains(result, "## assistant (turn 2)") {
			t.Errorf("expected assistant turn, got %q", result)
		}
		if !strings.Contains(result, "## user (turn 3)") {
			t.Errorf("expected second user turn, got %q", result)
		}
	})
}
