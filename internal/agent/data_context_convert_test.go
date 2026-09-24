package agent

import (
	"context"
	"testing"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// testLogger is a no-op logger for tests.
type testLogger struct{}

func (l *testLogger) Debug(ctx context.Context, msg string, fields map[string]any) {}
func (l *testLogger) Info(ctx context.Context, msg string, fields map[string]any)  {}
func (l *testLogger) Warn(ctx context.Context, msg string, fields map[string]any)  {}
func (l *testLogger) Error(ctx context.Context, msg string, fields map[string]any) {}

func TestGroupAssistantByResponse(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	logger := &testLogger{}

	tests := []struct {
		name     string
		input    []*turnagent.Message
		expected []*turnagent.Message
	}{
		{
			name: "SingleToolCall",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result", ToolCallID: "call_1", CreatedAt: now, TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result", ToolCallID: "call_1", CreatedAt: now, TurnID: "turn-1"},
			},
		},
		{
			name: "InterleavedMultiResponse",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 1", Content: "", CreatedAt: now, TurnID: "turn-1", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_A", Name: "read", Arguments: `{}`}}, CreatedAt: now.Add(10 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result A", ToolCallID: "call_A", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_B", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(100 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result B", ToolCallID: "call_B", CreatedAt: now.Add(120 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking 1", ToolCalls: []turnagent.ToolCall{{ID: "call_A", Name: "read", Arguments: `{}`}, {ID: "call_B", Name: "write", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result A", ToolCallID: "call_A", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result B", ToolCallID: "call_B", CreatedAt: now.Add(120 * time.Millisecond), TurnID: "turn-1"},
			},
		},
		{
			name: "BatchPattern",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: "", CreatedAt: now, TurnID: "turn-1", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_A", Name: "read", Arguments: `{}`}}, CreatedAt: now.Add(5 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_B", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(10 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result A", ToolCallID: "call_A", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result B", ToolCallID: "call_B", CreatedAt: now.Add(30 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", ToolCalls: []turnagent.ToolCall{{ID: "call_A", Name: "read", Arguments: `{}`}, {ID: "call_B", Name: "write", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result A", ToolCallID: "call_A", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result B", ToolCallID: "call_B", CreatedAt: now.Add(30 * time.Millisecond), TurnID: "turn-1"},
			},
		},
		{
			name: "EmptyTurnID",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_2", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(10 * time.Millisecond)},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_2", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(10 * time.Millisecond)},
			},
		},
		{
			name: "ToolCallOrdering",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: "", CreatedAt: now, TurnID: "turn-1", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_read", Name: "read", Arguments: `{"path":"/a"}`}}, CreatedAt: now.Add(5 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_ls", Name: "ls", Arguments: `{"path":"/b"}`}}, CreatedAt: now.Add(10 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", ToolCalls: []turnagent.ToolCall{{ID: "call_ls", Name: "ls", Arguments: `{"path":"/b"}`}, {ID: "call_read", Name: "read", Arguments: `{"path":"/a"}`}}, CreatedAt: now, TurnID: "turn-1"},
			},
		},
		{
			name: "DoesNotMutateOriginal",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: "", CreatedAt: now, TurnID: "turn-1", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now.Add(5 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
			},
		},
		{
			name: "TimeGapFallback",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_A", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result A", ToolCallID: "call_A", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_B", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(2000 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result B", ToolCallID: "call_B", CreatedAt: now.Add(2020 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_A", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result A", ToolCallID: "call_A", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_B", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(2000 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "result B", ToolCallID: "call_B", CreatedAt: now.Add(2020 * time.Millisecond), TurnID: "turn-1"},
			},
		},
		{
			name: "AbsorbedThinkingCleaned",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: "", CreatedAt: now, TurnID: "turn-1", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now.Add(5 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
			},
		},
		{
			name: "MultipleTurns",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking A", Content: "", CreatedAt: now, TurnID: "turn-A", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_A1", Name: "read", Arguments: `{}`}}, CreatedAt: now.Add(5 * time.Millisecond), TurnID: "turn-A"},
				{Role: turnagent.RoleTool, Content: "result A1", ToolCallID: "call_A1", CreatedAt: now.Add(10 * time.Millisecond), TurnID: "turn-A"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking B", Content: "", CreatedAt: now.Add(1000 * time.Millisecond), TurnID: "turn-B", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_B1", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(1005 * time.Millisecond), TurnID: "turn-B"},
				{Role: turnagent.RoleTool, Content: "result B1", ToolCallID: "call_B1", CreatedAt: now.Add(1010 * time.Millisecond), TurnID: "turn-B"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking A", ToolCalls: []turnagent.ToolCall{{ID: "call_A1", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-A"},
				{Role: turnagent.RoleTool, Content: "result A1", ToolCallID: "call_A1", CreatedAt: now.Add(10 * time.Millisecond), TurnID: "turn-A"},
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking B", ToolCalls: []turnagent.ToolCall{{ID: "call_B1", Name: "write", Arguments: `{}`}}, CreatedAt: now.Add(1000 * time.Millisecond), TurnID: "turn-B"},
				{Role: turnagent.RoleTool, Content: "result B1", ToolCallID: "call_B1", CreatedAt: now.Add(1010 * time.Millisecond), TurnID: "turn-B"},
			},
		},
		{
			name: "MixedTextAndToolCalls",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: "", CreatedAt: now, TurnID: "turn-1", Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}},
				{Role: turnagent.RoleAssistant, Content: "Let me analyze this", CreatedAt: now.Add(5 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now.Add(10 * time.Millisecond), TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "file content", ToolCallID: "call_1", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking", Content: "Let me analyze this", ToolCalls: []turnagent.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}, CreatedAt: now, TurnID: "turn-1"},
				{Role: turnagent.RoleTool, Content: "file content", ToolCallID: "call_1", CreatedAt: now.Add(20 * time.Millisecond), TurnID: "turn-1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// For DoesNotMutateOriginal, save original state before calling.
			var originalInput []*turnagent.Message
			if tt.name == "DoesNotMutateOriginal" {
				originalInput = make([]*turnagent.Message, len(tt.input))
				for i, msg := range tt.input {
					copied := *msg
					if msg.ToolCalls != nil {
						copied.ToolCalls = make([]turnagent.ToolCall, len(msg.ToolCalls))
						copy(copied.ToolCalls, msg.ToolCalls)
					}
					if msg.Extra != nil {
						copied.Extra = make(map[string]any, len(msg.Extra))
						for k, v := range msg.Extra {
							copied.Extra[k] = v
						}
					}
					originalInput[i] = &copied
				}
			}

			result := groupAssistantByResponse(ctx, logger, tt.input)

			// Verify DoesNotMutateOriginal.
			if tt.name == "DoesNotMutateOriginal" {
				for i, msg := range tt.input {
					orig := originalInput[i]
					if len(msg.ToolCalls) != len(orig.ToolCalls) {
						t.Errorf("input[%d].ToolCalls length mutated: got %d, want %d", i, len(msg.ToolCalls), len(orig.ToolCalls))
					}
					if msg.Extra != nil && orig.Extra != nil {
						if _, ok := msg.Extra[turnagent.ExtraKeyAbsorbedThinking]; !ok {
							if _, origOK := orig.Extra[turnagent.ExtraKeyAbsorbedThinking]; origOK {
								t.Errorf("input[%d].Extra[AbsorbedThinking] was removed (mutation detected)", i)
							}
						}
					}
				}
			}

			// Verify AbsorbedThinkingCleaned.
			if tt.name == "AbsorbedThinkingCleaned" {
				for i, msg := range result {
					if msg.Role == turnagent.RoleAssistant && msg.Extra != nil {
						if _, ok := msg.Extra[turnagent.ExtraKeyAbsorbedThinking]; ok {
							t.Errorf("result[%d].Extra[AbsorbedThinking] should be cleaned, but found", i)
						}
					}
				}
			}

			// Basic length check.
			if len(result) != len(tt.expected) {
				t.Errorf("length mismatch: got %d messages, want %d", len(result), len(tt.expected))
				t.Logf("got: %+v", result)
				t.Logf("want: %+v", tt.expected)
				return
			}

			// Detailed field comparison.
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
				if got.TurnID != want.TurnID {
					t.Errorf("message[%d].TurnID = %q, want %q", i, got.TurnID, want.TurnID)
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

func TestIsResponseBoundary(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name              string
		msg               *turnagent.Message
		lastAssistantTime time.Time
		expected          bool
	}{
		{
			name:              "AbsorbedThinkingMarker",
			msg:               &turnagent.Message{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1"}}, Extra: map[string]any{turnagent.ExtraKeyAbsorbedThinking: true}, CreatedAt: now},
			lastAssistantTime: now.Add(-10 * time.Millisecond),
			expected:          true,
		},
		{
			name:              "TimeGap",
			msg:               &turnagent.Message{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "call_1"}}, CreatedAt: now},
			lastAssistantTime: now.Add(-600 * time.Millisecond),
			expected:          true,
		},
		{
			name:              "NoSignal",
			msg:               &turnagent.Message{Role: turnagent.RoleAssistant, Content: "text", CreatedAt: now},
			lastAssistantTime: now.Add(-10 * time.Millisecond),
			expected:          false,
		},
		{
			name:              "ThinkingOnlyDefensive",
			msg:               &turnagent.Message{Role: turnagent.RoleAssistant, ReasoningContent: "thinking...", Content: "", CreatedAt: now},
			lastAssistantTime: now.Add(-10 * time.Millisecond),
			expected:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isResponseBoundary(tt.msg, tt.lastAssistantTime)
			if result != tt.expected {
				t.Errorf("isResponseBoundary() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestMergeAssistantMessages_AbsorbedThinkingMarker(t *testing.T) {
	tests := []struct {
		name     string
		input    []*turnagent.Message
		expected []*turnagent.Message
		checkFn  func(t *testing.T, result []*turnagent.Message)
	}{
		{
			name: "ThinkingMergedIntoToolCall",
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
			checkFn: func(t *testing.T, result []*turnagent.Message) {
				if len(result) != 1 {
					t.Fatalf("expected 1 message, got %d", len(result))
				}
				msg := result[0]
				if msg.Extra == nil {
					t.Errorf("Extra is nil, want non-nil with AbsorbedThinking marker")
				} else if msg.Extra[turnagent.ExtraKeyAbsorbedThinking] != true {
					t.Errorf("Extra[AbsorbedThinking] = %v, want true", msg.Extra[turnagent.ExtraKeyAbsorbedThinking])
				}
			},
		},
		{
			name: "ThinkingMergedIntoText",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "let me think...", Content: ""},
				{Role: turnagent.RoleAssistant, Content: "here is my answer"},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "here is my answer", ReasoningContent: "let me think..."},
			},
			checkFn: func(t *testing.T, result []*turnagent.Message) {
				if len(result) != 1 {
					t.Fatalf("expected 1 message, got %d", len(result))
				}
				msg := result[0]
				if msg.Extra == nil {
					t.Errorf("Extra is nil, want non-nil with AbsorbedThinking marker")
				} else if msg.Extra[turnagent.ExtraKeyAbsorbedThinking] != true {
					t.Errorf("Extra[AbsorbedThinking] = %v, want true", msg.Extra[turnagent.ExtraKeyAbsorbedThinking])
				}
			},
		},
		{
			name: "NoThinkingNoMarker",
			input: []*turnagent.Message{
				{
					Role:    turnagent.RoleAssistant,
					Content: "",
					ToolCalls: []turnagent.ToolCall{
						{ID: "call_A", Name: "read", Arguments: `{}`},
					},
				},
				{
					Role:    turnagent.RoleAssistant,
					Content: "",
					ToolCalls: []turnagent.ToolCall{
						{ID: "call_B", Name: "write", Arguments: `{}`},
					},
				},
			},
			expected: []*turnagent.Message{
				{
					Role:    turnagent.RoleAssistant,
					Content: "",
					ToolCalls: []turnagent.ToolCall{
						{ID: "call_A", Name: "read", Arguments: `{}`},
					},
				},
				{
					Role:    turnagent.RoleAssistant,
					Content: "",
					ToolCalls: []turnagent.ToolCall{
						{ID: "call_B", Name: "write", Arguments: `{}`},
					},
				},
			},
			checkFn: func(t *testing.T, result []*turnagent.Message) {
				for i, msg := range result {
					if msg.Extra != nil {
						if _, ok := msg.Extra[turnagent.ExtraKeyAbsorbedThinking]; ok {
							t.Errorf("message[%d].Extra[AbsorbedThinking] should not be set", i)
						}
					}
				}
			},
		},
		{
			name: "StandaloneThinkingConverted",
			input: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, ReasoningContent: "thinking alone", Content: ""},
			},
			expected: []*turnagent.Message{
				{Role: turnagent.RoleAssistant, Content: "thinking alone", ReasoningContent: ""},
			},
			checkFn: func(t *testing.T, result []*turnagent.Message) {
				if len(result) != 1 {
					t.Fatalf("expected 1 message, got %d", len(result))
				}
				msg := result[0]
				if msg.Extra != nil {
					if _, ok := msg.Extra[turnagent.ExtraKeyAbsorbedThinking]; ok {
						t.Errorf("standalone thinking should not have AbsorbedThinking marker")
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeAssistantMessages(tt.input)

			// Basic length check.
			if len(result) != len(tt.expected) {
				t.Errorf("length mismatch: got %d messages, want %d", len(result), len(tt.expected))
				t.Logf("got: %+v", result)
				t.Logf("want: %+v", tt.expected)
				return
			}

			// Field comparison.
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
					}
				}
			}

			// Run custom check function.
			if tt.checkFn != nil {
				tt.checkFn(t, result)
			}
		})
	}
}
