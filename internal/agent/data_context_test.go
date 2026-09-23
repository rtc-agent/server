package agent

import (
	"testing"

	"github.com/rtc-agent/server/internal/model"
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
					Role:             turnagent.RoleAssistant,
					Content:          "",
					ReasoningContent: "I should use a tool",
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
					Role:             turnagent.RoleAssistant,
					Content:          "final answer",
					ReasoningContent: "first thought\nsecond thought\nthird thought",
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
			name: "user message between thinking and text - thinking dropped",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: ""},
				{Role: turnagent.RoleUser, Content: "user interruption"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "user interruption"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "tool message between thinking and text - thinking dropped",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: ""},
				{Role: turnagent.RoleTool, Content: "tool result", ToolCallID: "call_1"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			expected: []*turnagent.Message{
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

			// Verify AbsorbedThinking marker is set for merged thinking cases.
			if tt.name == "thinking followed by text - merged" || tt.name == "thinking followed by tool call - merged" {
				for _, msg := range result {
					if msg.Role == turnagent.RoleAssistant && msg.ReasoningContent != "" {
						if msg.Extra == nil {
							t.Errorf("message.Extra is nil, want non-nil with AbsorbedThinking marker")
						} else if msg.Extra[turnagent.ExtraKeyAbsorbedThinking] != true {
							t.Errorf("message.Extra[%q] = %v, want true", turnagent.ExtraKeyAbsorbedThinking, msg.Extra[turnagent.ExtraKeyAbsorbedThinking])
						}
					}
				}
			}
		})
	}
}

func TestMergeAssistantMessages_NilInput(t *testing.T) {
	// mergeAssistantMessages must tolerate nil entries in the input slice
	// (which can occur when convertDBMessage returns nil for unparseable
	// content). Nil entries should be skipped without panic.
	input := []*turnagent.Message{
		{Role: turnagent.RoleUser, Content: "hello"},
		nil,
		{Role: turnagent.RoleAssistant, Content: "hi"},
	}
	result := mergeAssistantMessages(input)
	if len(result) != 2 {
		t.Errorf("expected 2 messages (nil skipped), got %d", len(result))
	}
	if len(result) >= 2 {
		if result[0].Content != "hello" {
			t.Errorf("result[0].Content = %q, want %q", result[0].Content, "hello")
		}
		if result[1].Content != "hi" {
			t.Errorf("result[1].Content = %q, want %q", result[1].Content, "hi")
		}
	}
}

func TestConvertDBMessage_EmptyContent(t *testing.T) {
	// convertDBMessage must handle empty or unparseable content gracefully.
	// It should return nil (not panic) for edge cases.
	msg := &model.Message{
		Content: "",
		Role:    "assistant",
	}
	result, _ := convertDBMessage(msg)
	if result != nil {
		t.Errorf("expected nil for empty content, got %v", result)
	}
}

func TestConvertDBMessage_UnknownType(t *testing.T) {
	// Unknown content type should return nil.
	msg := &model.Message{
		Content: `{"type":"unknown_type","data":"something"}`,
		Role:    "assistant",
	}
	result, _ := convertDBMessage(msg)
	if result != nil {
		t.Errorf("expected nil for unknown type, got %v", result)
	}
}

func TestConvertDBMessage_SkipsErrorType(t *testing.T) {
	// Error content type must be skipped by convertDBMessage — error messages
	// are stored for UI display but never sent to the LLM context.
	// See error_feedback.go: "The message does NOT enter the LLM context
	// (convertDBMessage's default branch skips error content type)."
	errorContent := `{"category":"system","title":"系统错误","message":"发生未知错误","retryable":false}`
	content := `{"type":"error","data":` + errorContent + `}`
	msg := &model.Message{
		Content: content,
		Role:    "assistant",
	}
	result, _ := convertDBMessage(msg)
	if result != nil {
		t.Errorf("expected nil for error content type (must not enter LLM context), got %d messages", len(result))
	}
}

func TestFilterMeaninglessThinking(t *testing.T) {
	tests := []struct {
		name     string
		input    []*turnagent.Message
		expected int // expected number of messages after filtering
	}{
		{
			name:     "empty input",
			input:    []*turnagent.Message{},
			expected: 0,
		},
		{
			name: "no thinking messages - all kept",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hello"},
				{Role: turnagent.RoleAssistant, Content: "hi there"},
			},
			expected: 2,
		},
		{
			name: "short thinking removed",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "...", Content: ""},
			},
			expected: 0,
		},
		{
			name: "very short thinking removed",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "ok", Content: ""},
			},
			expected: 0,
		},
		{
			name: "long thinking kept",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "This is a longer reasoning content that should be preserved", Content: ""},
			},
			expected: 1,
		},
		{
			name: "thinking with content kept regardless of length",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "short", Content: "has text content"},
			},
			expected: 1,
		},
		{
			name: "thinking with tool calls kept regardless of length",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "x", Content: "", ToolCalls: []turnagent.ToolCall{{ID: "1"}}},
			},
			expected: 1,
		},
		{
			name: "user message never filtered",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, ReasoningContent: "", Content: ""},
			},
			expected: 1,
		},
		{
			name: "nil messages skipped",
			input: []*turnagent.Message{
				nil,
				{Role: turnagent.RoleUser, Content: "hello"},
				nil,
			},
			expected: 1,
		},
		{
			name: "mixed scenario",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "question"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "...", Content: ""},                   // filtered
				{Role: turnagent.RoleAssistant, ReasoningContent: "[no content]", Content: ""},          // filtered
				{Role: turnagent.RoleAssistant, ReasoningContent: "valid reasoning here!", Content: ""}, // kept (>= 20 chars)
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			expected: 3, // user + valid thinking + answer
		},
		{
			name: "whitespace-only thinking removed",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "   \n\t  ", Content: ""},
			},
			expected: 0,
		},
		{
			name: "exactly 19 chars thinking removed",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "1234567890123456789", Content: ""},
			},
			expected: 0,
		},
		{
			name: "exactly 20 chars thinking kept",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "12345678901234567890", Content: ""},
			},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterMeaninglessThinking(tt.input)
			if len(result) != tt.expected {
				t.Errorf("filterMeaninglessThinking() returned %d messages, want %d", len(result), tt.expected)
				t.Logf("result: %+v", result)
			}
		})
	}
}

func TestSanitizeThinkTagLeak(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no think tags - unchanged",
			input:    "Hello, world!",
			expected: "Hello, world!",
		},
		{
			name:     "complete think tag removed",
			input:    "<think>internal reasoning</think>Actual answer",
			expected: "Actual answer",
		},
		{
			name:     "multiline think tag removed",
			input:    "<think>\nline 1\nline 2\n</think>Answer",
			expected: "Answer",
		},
		{
			name:     "think tag with trailing whitespace removed",
			input:    "<think>reasoning</think>   Answer",
			expected: "Answer",
		},
		{
			name:     "case insensitive",
			input:    "<THINK>reasoning</THINK>Answer",
			expected: "Answer",
		},
		{
			name:     "multiple think tags all removed",
			input:    "<think>first</think>middle<think>second</think>end",
			expected: "middleend",
		},
		{
			name:     "empty think tag removed",
			input:    "<think></think>Answer",
			expected: "Answer",
		},
		{
			name:     "incomplete think tag not removed (no closing)",
			input:    "<think>no closing tag",
			expected: "<think>no closing tag",
		},
		{
			name:     "empty input",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeThinkTagLeak(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeThinkTagLeak(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
