package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestCalculateRetentionIndex(t *testing.T) {
	// Helper to create a message with given content length.
	makeMsg := func(role schema.RoleType, contentLen int) *schema.Message {
		content := ""
		if contentLen > 0 {
			content = strings.Repeat("x", contentLen)
		}
		return &schema.Message{
			Role:    role,
			Content: content,
		}
	}

	tests := []struct {
		name           string
		msgs           []*schema.Message
		config         RetentionConfig
		expectedIndex  int
		description    string
	}{
		{
			name: "empty messages",
			msgs: []*schema.Message{},
			config: RetentionConfig{
				MinTokens:           100,
				MinTextBlockMessages: 2,
				MaxTokens:           1000,
			},
			expectedIndex: 0,
			description:   "should return 0 for empty messages",
		},
		{
			name: "single message below threshold",
			msgs: []*schema.Message{
				makeMsg(schema.User, 100), // ~25 tokens
			},
			config: RetentionConfig{
				MinTokens:           100,
				MinTextBlockMessages: 2,
				MaxTokens:           1000,
			},
			expectedIndex: 1, // all messages fit in retention budget
			description:   "should return len(msgs) when all fit in budget",
		},
		{
			name: "meets min tokens and min text blocks",
			msgs: []*schema.Message{
				makeMsg(schema.User, 400),      // ~100 tokens
				makeMsg(schema.Assistant, 400), // ~100 tokens
				makeMsg(schema.User, 400),      // ~100 tokens
				makeMsg(schema.Assistant, 400), // ~100 tokens
				makeMsg(schema.User, 400),      // ~100 tokens (total ~500 tokens, 5 text blocks)
			},
			config: RetentionConfig{
				MinTokens:           400, // need ~400 tokens
				MinTextBlockMessages: 3,  // need 3 text blocks
				MaxTokens:           2000,
			},
			// Walk from end: msg4(100) + msg3(100) + msg2(100) = 300 tokens, 3 text blocks
			// Still need more tokens. msg1(100) = 400 tokens, 4 text blocks. Met both conditions.
			// Index should be 1 (keep msgs 1-4).
			expectedIndex: 1,
			description:   "should find boundary when both min conditions met",
		},
		{
			name: "hits max tokens limit",
			msgs: []*schema.Message{
				makeMsg(schema.User, 4000),      // ~1000 tokens
				makeMsg(schema.Assistant, 4000), // ~1000 tokens
				makeMsg(schema.User, 4000),      // ~1000 tokens
				makeMsg(schema.Assistant, 4000), // ~1000 tokens
				makeMsg(schema.User, 4000),      // ~1000 tokens
			},
			config: RetentionConfig{
				MinTokens:           10000, // never met
				MinTextBlockMessages: 10,   // never met
				MaxTokens:           3000,  // hit after 3 messages from end
			},
			// Walk from end: msg4(1000) + msg3(1000) + msg2(1000) = 3000 tokens. Hit max.
			// Index should be 2 (keep msgs 2-4).
			expectedIndex: 2,
			description:   "should stop at max tokens even if min not met",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateRetentionIndex(tt.msgs, tt.config)
			if result != tt.expectedIndex {
				t.Errorf("%s: got %d, want %d", tt.description, result, tt.expectedIndex)
			}
		})
	}
}

func TestAdjustIndexToPreserveAPIInvariants(t *testing.T) {
	t.Run("tool_result at boundary pulls in assistant", func(t *testing.T) {
		msgs := []*schema.Message{
			{Role: schema.User, Content: "hello"},
			{Role: schema.Assistant, Content: "", ToolCalls: []schema.ToolCall{
				{ID: "call_1", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			}},
			{Role: schema.Tool, Content: "file content", ToolCallID: "call_1", ToolName: "read"},
			{Role: schema.Assistant, Content: "response"},
			{Role: schema.User, Content: "thanks"},
		}

		// If retention starts at index 2 (the tool message), the corresponding
		// assistant at index 1 should be pulled in to preserve the pairing.
		result := adjustIndexToPreserveAPIInvariants(msgs, 2)
		if result != 1 {
			t.Errorf("expected index 1 to include tool_use, got %d", result)
		}
	})

	t.Run("no adjustment needed when tool pair fully retained", func(t *testing.T) {
		msgs := []*schema.Message{
			{Role: schema.User, Content: "hello"},
			{Role: schema.Assistant, Content: "", ToolCalls: []schema.ToolCall{
				{ID: "call_1", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			}},
			{Role: schema.Tool, Content: "file content", ToolCallID: "call_1", ToolName: "read"},
			{Role: schema.Assistant, Content: "response"},
			{Role: schema.User, Content: "thanks"},
		}

		// If retention starts at index 1, both assistant and tool are retained.
		result := adjustIndexToPreserveAPIInvariants(msgs, 1)
		if result != 1 {
			t.Errorf("expected index 1 unchanged, got %d", result)
		}
	})

	t.Run("no adjustment needed without tool calls", func(t *testing.T) {
		msgs := []*schema.Message{
			{Role: schema.User, Content: "hello"},
			{Role: schema.Assistant, Content: "response"},
			{Role: schema.User, Content: "thanks"},
		}

		result := adjustIndexToPreserveAPIInvariants(msgs, 2)
		if result != 2 {
			t.Errorf("expected index 2 unchanged, got %d", result)
		}
	})

	t.Run("start at 0 returns 0", func(t *testing.T) {
		msgs := []*schema.Message{
			{Role: schema.User, Content: "hello"},
		}

		result := adjustIndexToPreserveAPIInvariants(msgs, 0)
		if result != 0 {
			t.Errorf("expected index 0, got %d", result)
		}
	})
}

func TestShouldCompressByTokenCount(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []*schema.Message
		expected bool
	}{
		{
			name:     "empty messages",
			msgs:     []*schema.Message{},
			expected: false,
		},
		{
			name: "single message",
			msgs: []*schema.Message{
				{Role: schema.User, Content: "hello"},
			},
			expected: false,
		},
		{
			name: "two messages",
			msgs: []*schema.Message{
				{Role: schema.User, Content: "hello"},
				{Role: schema.Assistant, Content: "hi"},
			},
			expected: false,
		},
		{
			name: "three messages",
			msgs: []*schema.Message{
				{Role: schema.User, Content: "hello"},
				{Role: schema.Assistant, Content: "hi"},
				{Role: schema.User, Content: "thanks"},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldCompressByTokenCount(tt.msgs)
			if result != tt.expected {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}
