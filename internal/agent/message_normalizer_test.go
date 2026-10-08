package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// testNoopLogger is a minimal Logger implementation for unit tests.
type testNoopLogger struct{}

func (testNoopLogger) Debug(_ context.Context, _ string, _ map[string]any) {}
func (testNoopLogger) Info(_ context.Context, _ string, _ map[string]any)  {}
func (testNoopLogger) Warn(_ context.Context, _ string, _ map[string]any)  {}
func (testNoopLogger) Error(_ context.Context, _ string, _ map[string]any) {}

// =============================================================================
// extractSystemMessages
// =============================================================================

func TestExtractSystemMessages(t *testing.T) {
	tests := []struct {
		name      string
		input     []*turnagent.Message
		wantRoles []string // expected role sequence
		wantSame  bool     // true → expect same pointer (no allocation)
	}{
		{
			name:     "empty",
			input:    nil,
			wantSame: true,
		},
		{
			name: "no system messages - returns original",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
			},
			wantRoles: []string{"user", "assistant"},
			wantSame:  true,
		},
		{
			name: "system at front - reordered",
			input: []*turnagent.Message{
				{Role: turnagent.RoleSystem, Content: "sys1"},
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
			},
			wantRoles: []string{"system", "user", "assistant"},
		},
		{
			name: "system in middle - extracted to front",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleSystem, Content: "sys1"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
			},
			wantRoles: []string{"system", "user", "assistant"},
		},
		{
			name: "multiple system messages - relative order preserved",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleSystem, Content: "sys1"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
				{Role: turnagent.RoleSystem, Content: "sys2"},
			},
			wantRoles: []string{"system", "system", "user", "assistant"},
		},
		{
			name: "only system messages",
			input: []*turnagent.Message{
				{Role: turnagent.RoleSystem, Content: "sys1"},
				{Role: turnagent.RoleSystem, Content: "sys2"},
			},
			wantRoles: []string{"system", "system"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.input
			result := extractSystemMessages(tt.input)

			if tt.wantSame {
				if len(original) == 0 {
					if len(result) != 0 {
						t.Error("expected empty result for empty input")
					}
					return
				}
				if &result[0] != &original[0] {
					t.Error("expected same slice pointer when no system messages")
				}
				return
			}

			if len(result) != len(tt.wantRoles) {
				t.Fatalf("got %d messages, want %d", len(result), len(tt.wantRoles))
			}
			for i, wantRole := range tt.wantRoles {
				if result[i].Role != wantRole {
					t.Errorf("result[%d].Role = %q, want %q", i, result[i].Role, wantRole)
				}
			}
		})
	}
}

// =============================================================================
// mergeConsecutiveSameRole
// =============================================================================

func TestMergeConsecutiveSameRole(t *testing.T) {
	tests := []struct {
		name         string
		input        []*turnagent.Message
		wantLen      int
		wantRoles    []string
		wantContents []string
	}{
		{
			name:    "empty",
			input:   nil,
			wantLen: 0,
		},
		{
			name: "single message",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"hi"},
		},
		{
			name: "no consecutive same role",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "a"},
				{Role: turnagent.RoleAssistant, Content: "b"},
				{Role: turnagent.RoleUser, Content: "c"},
			},
			wantLen:      3,
			wantRoles:    []string{"user", "assistant", "user"},
			wantContents: []string{"a", "b", "c"},
		},
		{
			name: "two consecutive user messages merged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hello"},
				{Role: turnagent.RoleUser, Content: "world"},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"hello\nworld"},
		},
		{
			name: "three consecutive assistant messages NOT merged (handled by middleware)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "a"},
				{Role: turnagent.RoleAssistant, Content: "b"},
				{Role: turnagent.RoleAssistant, Content: "c"},
			},
			wantLen:      3, // Assistant messages are NOT merged by normalize anymore
			wantRoles:    []string{"assistant", "assistant", "assistant"},
			wantContents: []string{"a", "b", "c"},
		},
		{
			name: "multiple groups merged (only user messages)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "u1"},
				{Role: turnagent.RoleUser, Content: "u2"},
				{Role: turnagent.RoleAssistant, Content: "a1"},
				{Role: turnagent.RoleAssistant, Content: "a2"},
				{Role: turnagent.RoleUser, Content: "u3"},
			},
			wantLen:      4, // User merged (2->1), assistant NOT merged (stays 2), user (1)
			wantRoles:    []string{"user", "assistant", "assistant", "user"},
			wantContents: []string{"u1\nu2", "a1", "a2", "u3"},
		},
		{
			name: "tool messages never merged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleTool, Content: "r1", ToolCallID: "c1"},
				{Role: turnagent.RoleTool, Content: "r2", ToolCallID: "c2"},
			},
			wantLen:      2,
			wantRoles:    []string{"tool", "tool"},
			wantContents: []string{"r1", "r2"},
		},
		{
			name: "system messages never merged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleSystem, Content: "s1"},
				{Role: turnagent.RoleSystem, Content: "s2"},
			},
			wantLen:      2,
			wantRoles:    []string{"system", "system"},
			wantContents: []string{"s1", "s2"},
		},
		{
			name: "empty content joined gracefully",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: ""},
				{Role: turnagent.RoleUser, Content: "hello"},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"hello"},
		},
		{
			name: "reasoning content also merged (user only)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "text", ReasoningContent: "think1"},
				{Role: turnagent.RoleUser, Content: "", ReasoningContent: "think2"},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"text"},
		},
		{
			name: "assistant text + assistant tool_use NOT merged (handled by middleware)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "我来创建任务"},
				{Role: turnagent.RoleAssistant, Content: "", ToolCalls: []turnagent.ToolCall{
					{ID: "call_1", Name: "createTask", Arguments: `{"title":"test"}`},
				}},
			},
			wantLen:      2, // Assistant messages are NOT merged by normalize anymore
			wantRoles:    []string{"assistant", "assistant"},
			wantContents: []string{"我来创建任务", ""},
		},
		{
			name: "multiple tool_use messages NOT merged (handled by middleware)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "", ToolCalls: []turnagent.ToolCall{
					{ID: "call_1", Name: "read", Arguments: `{"path":"a.txt"}`},
				}},
				{Role: turnagent.RoleAssistant, Content: "", ToolCalls: []turnagent.ToolCall{
					{ID: "call_2", Name: "read", Arguments: `{"path":"b.txt"}`},
				}},
			},
			wantLen:      2, // Assistant messages are NOT merged by normalize anymore
			wantRoles:    []string{"assistant", "assistant"},
			wantContents: []string{"", ""},
		},
		{
			name: "two consecutive user messages with MultiContent merged",
			input: []*turnagent.Message{
				{
					Role:    turnagent.RoleUser,
					Content: "first message",
					MultiContent: []schema.MessageInputPart{
						{Type: schema.ChatMessagePartTypeText, Text: "text1"},
					},
				},
				{
					Role:    turnagent.RoleUser,
					Content: "second message",
					MultiContent: []schema.MessageInputPart{
						{Type: schema.ChatMessagePartTypeText, Text: "text2"},
					},
				},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"first message\nsecond message"},
		},
		{
			name: "first user has MultiContent, second does not",
			input: []*turnagent.Message{
				{
					Role:    turnagent.RoleUser,
					Content: "first",
					MultiContent: []schema.MessageInputPart{
						{Type: schema.ChatMessagePartTypeText, Text: "text1"},
					},
				},
				{
					Role:    turnagent.RoleUser,
					Content: "second",
				},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"first\nsecond"},
		},
		{
			name: "first user has no MultiContent, second has MultiContent",
			input: []*turnagent.Message{
				{
					Role:    turnagent.RoleUser,
					Content: "first",
				},
				{
					Role:    turnagent.RoleUser,
					Content: "second",
					MultiContent: []schema.MessageInputPart{
						{Type: schema.ChatMessagePartTypeText, Text: "text2"},
					},
				},
			},
			wantLen:      1,
			wantRoles:    []string{"user"},
			wantContents: []string{"first\nsecond"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeConsecutiveSameRole(tt.input)

			if len(result) != tt.wantLen {
				t.Fatalf("got %d messages, want %d", len(result), tt.wantLen)
			}

			for i, wantRole := range tt.wantRoles {
				if result[i].Role != wantRole {
					t.Errorf("result[%d].Role = %q, want %q", i, result[i].Role, wantRole)
				}
			}
			for i, wantContent := range tt.wantContents {
				if result[i].Content != wantContent {
					t.Errorf("result[%d].Content = %q, want %q", i, result[i].Content, wantContent)
				}
			}
		})
	}
}

func TestMergeConsecutiveSameRole_MultiContent(t *testing.T) {
	t.Run("two consecutive user messages each with MultiContent", func(t *testing.T) {
		input := []*turnagent.Message{
			{
				Role:    turnagent.RoleUser,
				Content: "first",
				MultiContent: []schema.MessageInputPart{
					{Type: schema.ChatMessagePartTypeText, Text: "text1"},
				},
			},
			{
				Role:    turnagent.RoleUser,
				Content: "second",
				MultiContent: []schema.MessageInputPart{
					{Type: schema.ChatMessagePartTypeText, Text: "text2"},
				},
			},
		}

		result := mergeConsecutiveSameRole(input)

		if len(result) != 1 {
			t.Fatalf("got %d messages, want 1", len(result))
		}
		if len(result[0].MultiContent) != 2 {
			t.Fatalf("got %d MultiContent parts, want 2", len(result[0].MultiContent))
		}
		if result[0].MultiContent[0].Text != "text1" {
			t.Errorf("MultiContent[0].Text = %q, want %q", result[0].MultiContent[0].Text, "text1")
		}
		if result[0].MultiContent[1].Text != "text2" {
			t.Errorf("MultiContent[1].Text = %q, want %q", result[0].MultiContent[1].Text, "text2")
		}
	})

	t.Run("backing array independence after merge", func(t *testing.T) {
		// Verify that modifying the merged message's MultiContent doesn't affect original messages
		orig1 := &turnagent.Message{
			Role:    turnagent.RoleUser,
			Content: "first",
			MultiContent: []schema.MessageInputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "text1"},
			},
		}
		orig2 := &turnagent.Message{
			Role:    turnagent.RoleUser,
			Content: "second",
			MultiContent: []schema.MessageInputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "text2"},
			},
		}
		input := []*turnagent.Message{orig1, orig2}

		result := mergeConsecutiveSameRole(input)

		// Modify the merged message's MultiContent
		if len(result) > 0 && len(result[0].MultiContent) > 0 {
			result[0].MultiContent[0].Text = "modified"
		}

		// Original messages should not be affected
		if orig1.MultiContent[0].Text != "text1" {
			t.Errorf("orig1.MultiContent[0].Text was modified to %q, should still be %q",
				orig1.MultiContent[0].Text, "text1")
		}
		if orig2.MultiContent[0].Text != "text2" {
			t.Errorf("orig2.MultiContent[0].Text was modified to %q, should still be %q",
				orig2.MultiContent[0].Text, "text2")
		}
	})
}

func TestMergeConsecutiveSameRole_NoMutation(t *testing.T) {
	// Verify that merge does NOT mutate the caller's original messages.
	orig1 := &turnagent.Message{Role: turnagent.RoleUser, Content: "hello"}
	orig2 := &turnagent.Message{Role: turnagent.RoleUser, Content: "world"}
	input := []*turnagent.Message{orig1, orig2}

	_ = mergeConsecutiveSameRole(input)

	if orig1.Content != "hello" {
		t.Errorf("orig1.Content was mutated to %q, want %q", orig1.Content, "hello")
	}
	if orig2.Content != "world" {
		t.Errorf("orig2.Content was mutated to %q, want %q", orig2.Content, "world")
	}
}

// =============================================================================
// validateMessageSequence
// =============================================================================

func TestValidateMessageSequence(t *testing.T) {
	tests := []struct {
		name    string
		input   []*turnagent.Message
		wantErr bool
	}{
		{
			name:  "empty",
			input: nil,
		},
		{
			name: "valid: user → assistant",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
			},
		},
		{
			name: "valid: system → user → assistant",
			input: []*turnagent.Message{
				{Role: turnagent.RoleSystem, Content: "sys"},
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
			},
		},
		{
			name: "valid: tool use chain",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "read file"},
				{Role: turnagent.RoleAssistant, Content: "", ToolCalls: []turnagent.ToolCall{{ID: "c1"}}},
				{Role: turnagent.RoleTool, Content: "content", ToolCallID: "c1"},
				{Role: turnagent.RoleAssistant, Content: "done"},
			},
		},
		{
			name: "valid: prefill (assistant first)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "I'll help"},
				{Role: turnagent.RoleUser, Content: "thanks"},
			},
		},
		{
			name: "valid: tool → user (human interrupt)",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "start"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1"}}},
				{Role: turnagent.RoleTool, ToolCallID: "c1"},
				{Role: turnagent.RoleUser, Content: "never mind"},
				{Role: turnagent.RoleAssistant, Content: "ok"},
			},
		},
		{
			name: "valid: multiple consecutive tool messages",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "read files"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}},
				{Role: turnagent.RoleTool, Content: "file1", ToolCallID: "c1"},
				{Role: turnagent.RoleTool, Content: "file2", ToolCallID: "c2"},
				{Role: turnagent.RoleTool, Content: "file3", ToolCallID: "c3"},
				{Role: turnagent.RoleAssistant, Content: "done"},
			},
		},
		{
			name: "invalid: system after conversation",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleSystem, Content: "late sys"},
			},
			wantErr: true,
		},
		{
			name: "invalid: tool as first message",
			input: []*turnagent.Message{
				{Role: turnagent.RoleTool, Content: "orphan", ToolCallID: "c1"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			wantErr: true,
		},
		{
			name: "valid: all system messages",
			input: []*turnagent.Message{
				{Role: turnagent.RoleSystem, Content: "s1"},
				{Role: turnagent.RoleSystem, Content: "s2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMessageSequence(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMessageSequence() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// =============================================================================
// validateToolPairing
// =============================================================================

func TestValidateToolPairing(t *testing.T) {
	tests := []struct {
		name    string
		input   []*turnagent.Message
		wantErr bool
	}{
		{
			name:  "empty",
			input: nil,
		},
		{
			name: "properly paired",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1"}, {ID: "c2"}}},
				{Role: turnagent.RoleTool, ToolCallID: "c1"},
				{Role: turnagent.RoleTool, ToolCallID: "c2"},
			},
		},
		{
			name: "orphan tool result",
			input: []*turnagent.Message{
				{Role: turnagent.RoleTool, ToolCallID: "c1"},
			},
			wantErr: true,
		},
		{
			name: "unmatched assistant call",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1"}}},
			},
			wantErr: true,
		},
		{
			name: "no tool messages",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "hello"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateToolPairing(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateToolPairing() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// =============================================================================
// repairToolPairing
// =============================================================================

func TestRepairToolPairing(t *testing.T) {
	tests := []struct {
		name          string
		input         []*turnagent.Message
		wantLen       int
		wantRoles     []string
		wantToolIDs   []string // expected ToolCallID per message ("" if none)
		wantCallCount []int    // expected len(ToolCalls) per message
	}{
		{
			name:    "empty",
			input:   nil,
			wantLen: 0,
		},
		{
			name: "already paired - unchanged",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1"}}},
				{Role: turnagent.RoleTool, Content: "result", ToolCallID: "c1"},
			},
			wantLen:       3,
			wantRoles:     []string{"user", "assistant", "tool"},
			wantToolIDs:   []string{"", "", "c1"},
			wantCallCount: []int{0, 1, 0},
		},
		{
			name: "orphan tool result - dropped",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleTool, Content: "orphan", ToolCallID: "c_missing"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			wantLen:   2,
			wantRoles: []string{"user", "assistant"},
		},
		{
			name: "unmatched assistant call, no content - assistant and tool dropped",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "start"},
				{Role: turnagent.RoleAssistant, Content: "", ToolCalls: []turnagent.ToolCall{
					{ID: "c1"}, {ID: "c2"},
				}},
				{Role: turnagent.RoleTool, Content: "result1", ToolCallID: "c1"},
				// c2 has no result
				{Role: turnagent.RoleUser, Content: "next"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			// Assistant dropped (partial match, no content).
			// tool{c1} also dropped (its calling assistant was dropped).
			wantLen:   3,
			wantRoles: []string{"user", "user", "assistant"},
		},
		{
			name: "unmatched assistant call, has content - calls stripped, tool dropped",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "start"},
				{Role: turnagent.RoleAssistant, Content: "thinking out loud", ToolCalls: []turnagent.ToolCall{
					{ID: "c1"}, {ID: "c2"},
				}},
				{Role: turnagent.RoleTool, Content: "result1", ToolCallID: "c1"},
				{Role: turnagent.RoleUser, Content: "next"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			// Assistant kept with content, calls stripped. Tool dropped.
			wantLen:       4,
			wantRoles:     []string{"user", "assistant", "user", "assistant"},
			wantCallCount: []int{0, 0, 0, 0},
		},
		{
			name: "orphan tool at conversation start - dropped",
			input: []*turnagent.Message{
				{Role: turnagent.RoleTool, Content: "orphan", ToolCallID: "c1"},
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
			wantLen:   2,
			wantRoles: []string{"user", "assistant"},
		},
		{
			name: "duplicate tool results - only first kept",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1"}}},
				{Role: turnagent.RoleTool, Content: "result1", ToolCallID: "c1"},
				{Role: turnagent.RoleTool, Content: "result2", ToolCallID: "c1"},
			},
			wantLen:       2,
			wantRoles:     []string{"assistant", "tool"},
			wantToolIDs:   []string{"", "c1"},
			wantCallCount: []int{1, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := repairToolPairing(tt.input)

			if tt.wantLen == 0 {
				if len(result) != 0 {
					t.Fatalf("expected empty result, got %d messages", len(result))
				}
				return
			}

			if len(result) != tt.wantLen {
				t.Fatalf("got %d messages, want %d", len(result), tt.wantLen)
			}

			for i, wantRole := range tt.wantRoles {
				if result[i].Role != wantRole {
					t.Errorf("result[%d].Role = %q, want %q", i, result[i].Role, wantRole)
				}
			}

			if tt.wantToolIDs != nil {
				for i, wantID := range tt.wantToolIDs {
					if result[i].ToolCallID != wantID {
						t.Errorf("result[%d].ToolCallID = %q, want %q", i, result[i].ToolCallID, wantID)
					}
				}
			}

			if tt.wantCallCount != nil {
				for i, wantCount := range tt.wantCallCount {
					gotCount := len(result[i].ToolCalls)
					if gotCount != wantCount {
						t.Errorf("result[%d].ToolCalls count = %d, want %d", i, gotCount, wantCount)
					}
				}
			}
		})
	}
}

// =============================================================================
// joinContent
// =============================================================================

func TestJoinContent(t *testing.T) {
	tests := []struct {
		a, b, want string
	}{
		{"", "", ""},
		{"", "b", "b"},
		{"a", "", "a"},
		{"a", "b", "a\nb"},
		{"hello", "world", "hello\nworld"},
	}

	for _, tt := range tests {
		got := joinContent(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("joinContent(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

// =============================================================================
// Integration: extract + repair + merge + validate pipeline
// =============================================================================

func TestFullNormalizationPipeline_Integration(t *testing.T) {
	// Verify that the full pipeline (extract → repair → merge → validate)
	// always produces valid sequences.
	// This mirrors the order in normalizeMessagesForLLM.
	runPipeline := func(messages []*turnagent.Message) []*turnagent.Message {
		messages = extractSystemMessages(messages)
		messages = repairToolPairing(messages)
		messages = mergeConsecutiveSameRole(messages)
		return messages
	}

	tests := []struct {
		name  string
		input []*turnagent.Message
	}{
		{
			name: "orphan tool at start",
			input: []*turnagent.Message{
				{Role: turnagent.RoleTool, Content: "orphan", ToolCallID: "c1"},
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "partial match drops tool results and merges consecutive users",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "start"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{
					{ID: "c1"}, {ID: "c2"},
				}},
				{Role: turnagent.RoleTool, ToolCallID: "c1"},
				{Role: turnagent.RoleUser, Content: "next"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "all orphans",
			input: []*turnagent.Message{
				{Role: turnagent.RoleTool, Content: "orphan1", ToolCallID: "c1"},
				{Role: turnagent.RoleTool, Content: "orphan2", ToolCallID: "c2"},
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "system message in middle",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "hi"},
				{Role: turnagent.RoleSystem, Content: "late system"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "consecutive user messages",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "a"},
				{Role: turnagent.RoleUser, Content: "b"},
				{Role: turnagent.RoleAssistant, Content: "answer"},
			},
		},
		{
			name: "complex: system interleaved + orphan tool + consecutive users",
			input: []*turnagent.Message{
				{Role: turnagent.RoleUser, Content: "start"},
				{Role: turnagent.RoleSystem, Content: "misplaced system"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{
					{ID: "c1"}, {ID: "c_unmatched"},
				}},
				{Role: turnagent.RoleTool, ToolCallID: "c1"},
				{Role: turnagent.RoleUser, Content: "next"},
				{Role: turnagent.RoleUser, Content: "follow-up"},
				{Role: turnagent.RoleAssistant, Content: "done"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runPipeline(tt.input)
			if err := validateMessageSequence(result); err != nil {
				t.Errorf("pipeline produced invalid sequence: %v", err)
				t.Logf("result messages:")
				for i, m := range result {
					t.Logf("  [%d] role=%s content=%q toolCallID=%q toolCalls=%d",
						i, m.Role, m.Content, m.ToolCallID, len(m.ToolCalls))
				}
			}
		})
	}
}

// =============================================================================
// normalizeSchemaMessagesForLLM — comprehensive table-driven tests
// =============================================================================

// TestNormalizeSchemaMessages_ComprehensiveTables verifies that
// normalizeSchemaMessagesForLLM correctly normalizes two comprehensive message
// tables: one without summary (pre-compression diversity) and one with summary
// (post-compression scenario — the exact bug case).
func TestNormalizeSchemaMessages_ComprehensiveTables(t *testing.T) {
	h := &helpers{logger: testNoopLogger{}}
	ctx := t.Context()
	// Use a dummy sessionID — normalizeSchemaMessagesForLLM only uses it for logging.
	sid := [16]byte{0x01}

	t.Run("table1_no_summary_all_message_types", func(t *testing.T) {
		// Table 1: Without summary. Contains all message types/roles the system
		// can produce, deliberately in a scrambled order that violates API
		// invariants. This exercises every normalization step.
		//
		// Input order (intentionally broken):
		//   [user, assistant(tool_call), system(scattered), tool(result),
		//    user, assistant(text), system(another), user, assistant(text)]
		//
		// Expected after normalization:
		//   [system, system, user, assistant(tool_call), tool(result),
		//    user, assistant(text), user, assistant(text)]
		input := []*schema.Message{
			{Role: schema.User, Content: "first user message"},
			{Role: schema.Assistant, Content: "let me check",
				ToolCalls: []schema.ToolCall{{ID: "tc1", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"/foo"}`}}}},
			{Role: schema.System, Content: "system prompt A"},
			{Role: schema.Tool, Content: "file contents", ToolCallID: "tc1"},
			{Role: schema.User, Content: "second user message"},
			{Role: schema.Assistant, Content: "here is the result"},
			{Role: schema.System, Content: "system prompt B"},
			{Role: schema.User, Content: "third user message"},
			{Role: schema.Assistant, Content: "final response"},
		}

		result, err := h.normalizeSchemaMessagesForLLM(ctx, sid, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify: system messages are leading.
		verifySystemLeading(t, result)

		// Verify: first non-system is user.
		verifyFirstNonSystemIsUser(t, result)

		// Verify: tool pairing is valid.
		verifyToolPairing(t, result)

		// Verify: no consecutive same-role user messages (they should be merged).
		verifyNoConsecutiveSameRoleUser(t, result)

		// Users are separated by tool/assistant, so no merging happens.
		// Total = 2 system + 3 user + 3 assistant + 1 tool = 9.
		if len(result) != 9 {
			t.Errorf("expected 9 messages after normalization, got %d", len(result))
			for i, m := range result {
				t.Logf("  [%d] role=%s content=%q", i, m.Role, truncateContent(m.Content, 40))
			}
		}
	})

	t.Run("table2_with_summary_post_compression_bug_scenario", func(t *testing.T) {
		// Table 2: The EXACT bug scenario. This is what buildCompressedResult
		// + appendPostCompactAttachments produces BEFORE normalization:
		//
		//   [summary(user), system(agent_prompt), system(session_memory),
		//    system(post_compact_attachment), assistant(retained), user(retained)]
		//
		// Without normalization: preProcessMessages sees user first, then system
		// messages are NOT extracted → API rejects with "first non-system message
		// should be user message" (because systems end up in messages array).
		//
		// After normalization: systems extracted to front →
		//   [system, system, system, summary(user), assistant, user]
		// preProcessMessages extracts leading systems → API parameter.
		// Messages array: [summary(user), assistant, user] → valid!
		input := []*schema.Message{
			// Summary (user role) — from buildCompressedResult
			{Role: schema.User, Content: "<summary>Conversation about file /foo.go...</summary>"},
			// System messages from discarded portion
			{Role: schema.System, Content: "You are a helpful assistant."},
			{Role: schema.System, Content: "SessionMemory: user prefers Go"},
			// Post-compact file recovery attachment (system-reminder)
			{Role: schema.System, Content: "<system-reminder>Recently read file: /foo.go\ncontent...</system-reminder>"},
			// Retained conversation messages
			{Role: schema.Assistant, Content: "I've read the file. Here's my analysis..."},
			{Role: schema.User, Content: "Thanks, now edit it."},
		}

		result, err := h.normalizeSchemaMessagesForLLM(ctx, sid, input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify: system messages are leading.
		verifySystemLeading(t, result)

		// Verify: first non-system is user (the summary).
		verifyFirstNonSystemIsUser(t, result)

		// Verify: system count = 3 (agent_prompt, session_memory, post_compact).
		systemCount := 0
		for _, m := range result {
			if m.Role == schema.System {
				systemCount++
			}
		}
		if systemCount != 3 {
			t.Errorf("expected 3 system messages, got %d", systemCount)
		}

		// Verify: total = 3 system + 1 summary(user) + 1 assistant + 1 user = 6.
		if len(result) != 6 {
			t.Errorf("expected 6 messages after normalization, got %d", len(result))
		}

		// Verify: exact expected order.
		expectedRoles := []schema.RoleType{schema.System, schema.System, schema.System, schema.User, schema.Assistant, schema.User}
		for i, want := range expectedRoles {
			if i >= len(result) {
				t.Fatalf("result too short: expected role %q at index %d, but only %d messages", want, i, len(result))
			}
			if result[i].Role != want {
				t.Errorf("index %d: expected role %q, got %q", i, want, result[i].Role)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// Test helpers for normalizeSchemaMessagesForLLM tests
// -----------------------------------------------------------------------------

func verifySystemLeading(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	inConversation := false
	for i, m := range msgs {
		if m.Role == schema.System {
			if inConversation {
				t.Errorf("system message at position %d after conversation start", i)
			}
		} else {
			inConversation = true
		}
	}
}

func verifyFirstNonSystemIsUser(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	for _, m := range msgs {
		if m.Role != schema.System {
			if m.Role != schema.User {
				t.Errorf("first non-system message has role %q, expected %q", m.Role, schema.User)
			}
			return
		}
	}
}

func verifyToolPairing(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	pending := 0
	for i, m := range msgs {
		switch m.Role {
		case schema.Assistant:
			pending += len(m.ToolCalls)
		case schema.Tool:
			if pending <= 0 {
				t.Errorf("tool message at position %d without matching assistant tool_call", i)
			}
			pending--
		}
	}
	if pending > 0 {
		t.Errorf("%d assistant tool_call(s) without matching tool result", pending)
	}
}

func verifyNoConsecutiveSameRoleUser(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == schema.User && msgs[i-1].Role == schema.User {
			t.Errorf("consecutive user messages at positions %d-%d (should be merged)", i-1, i)
		}
	}
}

func truncateContent(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
