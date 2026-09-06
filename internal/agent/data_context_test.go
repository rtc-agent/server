package agent

import (
	"testing"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestMergeAssistantMessages(t *testing.T) {
	tests := []struct {
		name     string
		input    []*turnagent.Message
		expected []*turnagent.Message
	}{
		{
			name:     "empty input",
			input:    []*turnagent.Message{},
			expected: []*turnagent.Message{},
		},
		{
			name: "no thinking messages - unchanged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hello"},
				{Role: turnagent.RoleAssistant, Content: "hi there"},
				{Role: turnagent.RoleUser, Content: "how are you?"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hello"},
				{Role: turnagent.RoleAssistant, Content: "hi there"},
				{Role: turnagent.RoleUser, Content: "how are you?"},
			},
		},
		{
			name: "thinking followed by text - merged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "let me think...", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "here is my answer"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "here is my answer", ReasoningContent: "let me think..."},
			},
		},
		{
			name: "thinking followed by tool call - merged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "I should use a tool", Content: ""},
				{
					Role:    turnagent.RoleAssistant,
					Content: "",
					ToolCalls: []turnagent.ToolCall{
						{ID: "call_1", Name: "search", Arguments: `{"query":"test"}`},
					},
				},
			},
			expected: []*turnagent.Message{
				{
					Role:               turnagent.RoleAssistant,
					Content:            "",
					ReasoningContent:   "I should use a tool",
					ToolCalls: []turnagent.ToolCall{
						{ID: "call_1", Name: "search", Arguments: `{"query":"test"}`},
					},
				},
			},
		},
		{
			name: "thinking followed by thinking then text - all accumulated",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "first thought", Content: ""},
				{Role: turnagent.RoleAssistant, ReasoningContent: "second thought", Content: ""},
				{Role: turnagent.RoleAssistant, ReasoningContent: "third thought", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "final answer"},
			},
			expected: []*turnagent.Message{
				{
					Role:               turnagent.RoleAssistant,
					Content:            "final answer",
					ReasoningContent:   "first thought\nsecond thought\nthird thought",
				},
			},
		},
		{
			name: "standalone thinking - converted to text",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking alone", Content: ""},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "thinking alone", ReasoningContent: ""},
			},
		},
		{
			name: "multiple thinking+text pairs - all merged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 1", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "text 1"},
				{Role: turnagent.RoleUser, Content: "follow up question"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 2", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "text 2"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "text 1", ReasoningContent: "thinking 1"},
				{Role: turnagent.RoleUser, Content: "follow up question"},
				{Role: turnagent.RoleAssistant, Content: "text 2", ReasoningContent: "thinking 2"},
			},
		},
		{
			name: "user message between thinking and text - no merge",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: ""},
				{Role: turnagent.RoleUser, Content: "user interruption"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "thinking", ReasoningContent: ""},
				{Role: turnagent.RoleUser, Content: "user interruption"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "tool message between thinking and text - no merge",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: ""},
				{Role: turnagent.RoleTool, Content: "tool result", ToolCallID: "call_1"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "thinking", ReasoningContent: ""},
				{Role: turnagent.RoleTool, Content: "tool result", ToolCallID: "call_1"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "mixed scenario - multiple patterns",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "question 1"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 1", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "answer 1"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 2", Content: ""},
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 3", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "answer 2"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "orphan thinking", Content: ""},
				{Role: turnagent.RoleUser, Content: "question 2"},
				{Role: turnagent.RoleAssistant, Content: "normal answer"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "question 1"},
				{Role: turnagent.RoleAssistant, Content: "answer 1", ReasoningContent: "thinking 1"},
				{Role: turnagent.RoleAssistant, Content: "answer 2", ReasoningContent: "thinking 2\nthinking 3"},
				{Role: turnagent.RoleAssistant, Content: "orphan thinking", ReasoningContent: ""},
				{Role: turnagent.RoleUser, Content: "question 2"},
				{Role: turnagent.RoleAssistant, Content: "normal answer"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeAssistantMessages(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("length mismatch: got %d messages, want %d", len(result), len(tt.expected))
				t.Logf("got: %+v", result)
				t.Logf("want: %+v", tt.expected)
				return
			}

			for i := range result {
				got := result[i]
				want := tt.expected[i]

				if got.Role != want.Role {
					t.Errorf("message[%d].Role = %q, want %q", i, got.Role, want.Role)
				}
				if got.Content != want.Content {
					t.Errorf("message[%d].Content = %q, want %q", i, got.Content, want.Content)
				}
				if got.ReasoningContent != want.ReasoningContent {
					t.Errorf("message[%d].ReasoningContent = %q, want %q", i, got.ReasoningContent, want.ReasoningContent)
				}
				if len(got.ToolCalls) != len(want.ToolCalls) {
					t.Errorf("message[%d].ToolCalls length = %d, want %d", i, len(got.ToolCalls), len(want.ToolCalls))
				} else {
					for j := range got.ToolCalls {
						if got.ToolCalls[j].ID != want.ToolCalls[j].ID {
							t.Errorf("message[%d].ToolCalls[%d].ID = %q, want %q", i, j, got.ToolCalls[j].ID, want.ToolCalls[j].ID)
						}
						if got.ToolCalls[j].Name != want.ToolCalls[j].Name {
							t.Errorf("message[%d].ToolCalls[%d].Name = %q, want %q", i, j, got.ToolCalls[j].Name, want.ToolCalls[j].Name)
						}
						if got.ToolCalls[j].Arguments != want.ToolCalls[j].Arguments {
							t.Errorf("message[%d].ToolCalls[%d].Arguments = %q, want %q", i, j, got.ToolCalls[j].Arguments, want.ToolCalls[j].Arguments)
						}
					}
				}
			}
		})
	}
}
