package agent

import (
	"context"
	"errors"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ---------------------------------------------------------------------------
// test helpers / mocks
// ---------------------------------------------------------------------------

type noopLogger struct{}

func (noopLogger) Debug(context.Context, string, map[string]any) {}
func (noopLogger) Info(context.Context, string, map[string]any)  {}
func (noopLogger) Warn(context.Context, string, map[string]any)  {}
func (noopLogger) Error(context.Context, string, map[string]any) {}

// mockSessionMemoryRepo is a minimal mock that only implements ListBySession;
// other methods panic to surface unexpected usage.
type mockSessionMemoryRepo struct {
	listBySessionFn func(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.SessionMemory, error)
}

var _ repo.SessionMemoryRepo = (*mockSessionMemoryRepo)(nil)

func (m *mockSessionMemoryRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, limit int) ([]*model.SessionMemory, error) {
	if m.listBySessionFn != nil {
		return m.listBySessionFn(ctx, sessionID, limit)
	}
	return nil, nil
}

func (*mockSessionMemoryRepo) Create(context.Context, *model.SessionMemory) error {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) BatchCreate(context.Context, []*model.SessionMemory) error {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) Update(context.Context, uuid.UUID, map[string]any) error {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) Delete(context.Context, uuid.UUID) error {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) GetByID(context.Context, uuid.UUID) (*model.SessionMemory, error) {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) ListByCategory(context.Context, uuid.UUID, string, int) ([]*model.SessionMemory, error) {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) ListRecentForInjection(context.Context, uuid.UUID, int, int) ([]*model.SessionMemory, error) {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) DeleteBySession(context.Context, uuid.UUID) error {
	panic("not implemented")
}
func (*mockSessionMemoryRepo) CountTokensBySession(context.Context, uuid.UUID) (int, error) {
	panic("not implemented")
}

// fixedTokenCounter builds a TokenCounterFunc that always returns the given value.
func fixedTokenCounter(n int, err error) turnagent.TokenCounterFunc {
	return func(ctx context.Context, messages []*schema.Message) (int, error) {
		return n, err
	}
}

// helper to build an assistant message with N tool calls.
func assistantWithToolCalls(n int) *schema.Message {
	msg := &schema.Message{Role: schema.Assistant, Content: "hi"}
	for i := 0; i < n; i++ {
		msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
			ID:       uuid.NewString(),
			Function: schema.FunctionCall{Name: "tool_" + uuid.NewString()[:4], Arguments: "{}"},
		})
	}
	return msg
}

// ---------------------------------------------------------------------------
// hasToolCallInLastAssistant
// ---------------------------------------------------------------------------

func TestHasToolCallInLastAssistant(t *testing.T) {
	tests := []struct {
		name     string
		messages []*schema.Message
		want     bool
	}{
		{
			name:     "empty messages",
			messages: nil,
			want:     false,
		},
		{
			name:     "no assistant messages",
			messages: []*schema.Message{{Role: schema.User, Content: "hi"}},
			want:     false,
		},
		{
			name: "last assistant has tool calls",
			messages: []*schema.Message{
				{Role: schema.Assistant, Content: "a", ToolCalls: []schema.ToolCall{{ID: "1", Function: schema.FunctionCall{Name: "t"}}}},
				{Role: schema.User, Content: "b"},
				{Role: schema.Assistant, Content: "c", ToolCalls: []schema.ToolCall{{ID: "2", Function: schema.FunctionCall{Name: "t"}}}},
			},
			want: true,
		},
		{
			name: "last assistant has no tool calls, earlier one does",
			messages: []*schema.Message{
				{Role: schema.Assistant, Content: "a", ToolCalls: []schema.ToolCall{{ID: "1", Function: schema.FunctionCall{Name: "t"}}}},
				{Role: schema.User, Content: "b"},
				{Role: schema.Assistant, Content: "c"},
			},
			want: false,
		},
		{
			name:     "single assistant with no tool calls",
			messages: []*schema.Message{{Role: schema.Assistant, Content: "hi"}},
			want:     false,
		},
		{
			name: "single assistant with tool calls",
			messages: []*schema.Message{
				{Role: schema.Assistant, Content: "hi", ToolCalls: []schema.ToolCall{{ID: "1", Function: schema.FunctionCall{Name: "t"}}}},
			},
			want: true,
		},
		{
			name: "mixed roles - user/tool/assistant with no calls on last",
			messages: []*schema.Message{
				{Role: schema.User, Content: "u"},
				{Role: schema.Assistant, Content: "a", ToolCalls: []schema.ToolCall{{ID: "1", Function: schema.FunctionCall{Name: "t"}}}},
				{Role: schema.Tool, Content: "result"},
				{Role: schema.User, Content: "u2"},
				{Role: schema.Assistant, Content: "last"},
			},
			want: false,
		},
		{
			name: "mixed roles - last assistant has tool call",
			messages: []*schema.Message{
				{Role: schema.User, Content: "u"},
				{Role: schema.Assistant, Content: "a"},
				{Role: schema.Tool, Content: "result"},
				{Role: schema.User, Content: "u2"},
				{Role: schema.Assistant, Content: "last", ToolCalls: []schema.ToolCall{{ID: "1", Function: schema.FunctionCall{Name: "t"}}}},
			},
			want: true,
		},
		{
			name: "last assistant has empty tool calls slice",
			messages: []*schema.Message{
				{Role: schema.Assistant, Content: "a", ToolCalls: []schema.ToolCall{}},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasToolCallInLastAssistant(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// countToolCalls
// ---------------------------------------------------------------------------

func TestCountToolCalls(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.User, Content: "u1"},
		{Role: schema.Assistant, Content: "a1", ToolCalls: []schema.ToolCall{{ID: "1"}, {ID: "2"}}}, // 2 calls
		{Role: schema.User, Content: "u2"},
		{Role: schema.Assistant, Content: "a2"}, // 0
		{Role: schema.User, Content: "u3"},
		{Role: schema.Assistant, Content: "a3", ToolCalls: []schema.ToolCall{{ID: "3"}}}, // 1 call
	}

	tests := []struct {
		name     string
		messages []*schema.Message
		state    *ExtractionState
		want     int
	}{
		{
			name:     "nil state - counts all",
			messages: messages,
			state:    nil,
			want:     3,
		},
		{
			name:     "state with LastMessageCount=3 - counts from index 3 forward",
			messages: messages,
			state:    &ExtractionState{LastMessageCount: 3},
			want:     1, // only the last assistant's 1 call
		},
		{
			name:     "state with LastMessageCount == len(messages) - counts all (condition fails, startIdx=0)",
			messages: messages,
			state:    &ExtractionState{LastMessageCount: len(messages)},
			want:     3,
		},
		{
			name:     "state with LastMessageCount=0 - counts all",
			messages: messages,
			state:    &ExtractionState{LastMessageCount: 0},
			want:     3,
		},
		{
			name:     "state with LastMessageCount greater than len - counts all (fallback to start)",
			messages: messages,
			state:    &ExtractionState{LastMessageCount: 100},
			want:     3,
		},
		{
			name:     "empty messages - 0",
			messages: nil,
			state:    nil,
			want:     0,
		},
		{
			name: "multiple assistant messages with varying calls - nil state",
			messages: []*schema.Message{
				assistantWithToolCalls(1),
				assistantWithToolCalls(3),
				assistantWithToolCalls(2),
			},
			state: nil,
			want:  6,
		},
		{
			name: "state LastMessageCount skips first two assistants",
			messages: []*schema.Message{
				assistantWithToolCalls(1),
				assistantWithToolCalls(3),
				{Role: schema.User, Content: "u"},
				assistantWithToolCalls(2),
			},
			state: &ExtractionState{LastMessageCount: 2},
			want:  2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countToolCalls(tc.messages, tc.state)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// truncateString
// ---------------------------------------------------------------------------

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{name: "short string unchanged", s: "hello", maxLen: 10, want: "hello"},
		{name: "exact length unchanged", s: "hello", maxLen: 5, want: "hello"},
		{name: "long string truncated", s: "hello world", maxLen: 5, want: "hello..."},
		{name: "empty string", s: "", maxLen: 5, want: ""},
		{name: "CJK runes truncate by rune not byte", s: "你好世界啊", maxLen: 3, want: "你好世..."},
		{name: "CJK within limit", s: "你好", maxLen: 5, want: "你好"},
		{name: "maxLen zero", s: "abc", maxLen: 0, want: "..."},
		{name: "mixed runes within limit", s: "a你好", maxLen: 3, want: "a你好"},
		{name: "mixed runes truncated", s: "a你好b", maxLen: 2, want: "a你..."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateString(tc.s, tc.maxLen)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// ExtractIfNeeded
// ---------------------------------------------------------------------------

func TestExtractIfNeeded(t *testing.T) {
	sessionID := uuid.New()

	// messages used to satisfy the tool-call / last-assistant checks.
	// 3 tool calls across messages, last assistant HAS tool call.
	messagesWithToolCalls := []*schema.Message{
		{Role: schema.User, Content: "u1"},
		assistantWithToolCalls(2),
		{Role: schema.User, Content: "u2"},
		assistantWithToolCalls(1),
	}

	// 3 tool calls, last assistant has NO tool call.
	messagesNoToolCallLast := []*schema.Message{
		{Role: schema.User, Content: "u1"},
		assistantWithToolCalls(3),
		{Role: schema.User, Content: "u2"},
		{Role: schema.Assistant, Content: "done"},
	}

	// 1 tool call only, last assistant HAS tool call.
	messagesFewToolCalls := []*schema.Message{
		{Role: schema.User, Content: "u1"},
		assistantWithToolCalls(1),
	}

	mockRepo := &mockSessionMemoryRepo{
		listBySessionFn: func(ctx context.Context, sid uuid.UUID, limit int) ([]*model.SessionMemory, error) {
			return nil, nil
		},
	}

	tests := []struct {
		name          string
		chatModelNil  bool
		tokenCount    int
		tokenErr      error
		state         *ExtractionState
		messages      []*schema.Message
		initThreshold int
		updThreshold  int
		minToolCalls  int
		wantExtracted bool
		wantErr       bool
	}{
		{
			name:          "chatModel nil returns false no error",
			chatModelNil:  true,
			tokenCount:    20000,
			messages:      messagesWithToolCalls,
			initThreshold: 10000,
			updThreshold:  5000,
			minToolCalls:  3,
			wantExtracted: false,
		},
		{
			name:          "token count below InitThreshold",
			tokenCount:    5000,
			messages:      messagesWithToolCalls,
			initThreshold: 10000,
			updThreshold:  5000,
			minToolCalls:  3,
			wantExtracted: false,
		},
		{
			name:          "above InitThreshold but nil state and no growth (tokenGrowth==currentTokens, below UpdateThreshold)",
			tokenCount:    12000, // >= init (10000), but growth (12000) >= upd (5000) => may trigger, so use 10000 exactly
			state:         nil,
			messages:      messagesWithToolCalls,
			initThreshold: 10000,
			updThreshold:  15000, // raise so growth 12000 < 15000
			minToolCalls:  3,
			wantExtracted: false,
		},
		{
			name:          "token growth >= UpdateThreshold AND tool calls >= MinToolCalls triggers",
			tokenCount:    20000,
			state:         &ExtractionState{LastTokenCount: 10000, LastMessageCount: 0},
			messages:      messagesWithToolCalls, // 3 tool calls, last assistant has tool call
			initThreshold: 10000,
			updThreshold:  5000,
			minToolCalls:  3,
			wantExtracted: true,
		},
		{
			name:          "token growth >= UpdateThreshold AND last assistant no tool call triggers",
			tokenCount:    20000,
			state:         &ExtractionState{LastTokenCount: 10000, LastMessageCount: 0},
			messages:      messagesNoToolCallLast, // 3 tool calls, last assistant no tool call
			initThreshold: 10000,
			updThreshold:  5000,
			minToolCalls:  10, // high min so first OR branch fails; second still matches
			wantExtracted: true,
		},
		{
			name:          "token growth >= UpdateThreshold but tool calls < MinToolCalls AND last assistant has tool call => no trigger",
			tokenCount:    20000,
			state:         &ExtractionState{LastTokenCount: 10000, LastMessageCount: 0},
			messages:      messagesFewToolCalls, // 1 tool call, last assistant has tool call
			initThreshold: 10000,
			updThreshold:  5000,
			minToolCalls:  3,
			wantExtracted: false,
		},
		{
			name:          "token counter error propagates",
			tokenCount:    0,
			tokenErr:      errors.New("count failed"),
			messages:      messagesWithToolCalls,
			initThreshold: 10000,
			wantErr:       true,
		},
		{
			name:          "exact InitThreshold triggers",
			tokenCount:    10000,
			state:         nil,
			messages:      messagesWithToolCalls,
			initThreshold: 10000,
			updThreshold:  5000,
			minToolCalls:  3,
			wantExtracted: true, // tokenGrowth=10000 >= 5000, tool calls 3 >= 3
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ext *SessionMemoryExtractor
			if tc.chatModelNil {
				ext = &SessionMemoryExtractor{
					chatModel:       nil,
					memoryRepo:      mockRepo,
					tokenCounter:    fixedTokenCounter(tc.tokenCount, tc.tokenErr),
					logger:          noopLogger{},
					InitThreshold:   tc.initThreshold,
					UpdateThreshold: tc.updThreshold,
					MinToolCalls:    tc.minToolCalls,
				}
			} else {
				// Use a non-nil chatModel. We don't actually invoke Stream in these
				// skip/trigger-decision tests except when wantExtracted=true, in which
				// case the real Stream call will fail. For trigger-logic tests that
				// want wantExtracted=true, we instead construct a stub chatModel via
				// a separate subtest path (see below).
				//
				// Here we handle the two cases:
				//   - wantExtracted=false: chatModel is only used for nil-check; any
				//     non-nil value works. Use a typed nil-free stub.
				//   - wantExtracted=true:  would call Stream; skip if the test would
				//     actually need to run the LLM. Instead, for pure trigger-decision
				//     validation, we check the trigger condition directly by expecting
				//     an error from Stream (acceptable here).
				ext = &SessionMemoryExtractor{
					chatModel:       &stubChatModel{err: errors.New("stub: not implemented")},
					memoryRepo:      mockRepo,
					tokenCounter:    fixedTokenCounter(tc.tokenCount, tc.tokenErr),
					logger:          noopLogger{},
					InitThreshold:   tc.initThreshold,
					UpdateThreshold: tc.updThreshold,
					MinToolCalls:    tc.minToolCalls,
				}
			}

			gotExtracted, newState, err := ext.ExtractIfNeeded(context.Background(), sessionID, tc.messages, tc.state)

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			if tc.chatModelNil {
				require.NoError(t, err)
				assert.False(t, gotExtracted)
				return
			}

			// For cases where extraction should trigger, Stream will fail with our stub.
			// That's expected; we still assert that the code path reached the extract
			// phase (i.e. did not short-circuit earlier).
			if tc.wantExtracted {
				// Stream will fail -> err != nil, extracted=false, but state is returned
				// as the original state (not new). Verify no early short-circuit occurred
				// by checking err came from extractMemories (Stream error).
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "stub")
			} else {
				require.NoError(t, err)
				assert.False(t, gotExtracted)
				assert.Equal(t, tc.state, newState)
			}
		})
	}
}

// stubChatModel is a minimal ToolCallingChatModel stub.
// It is only used to satisfy the nil-check in ExtractIfNeeded; Stream always errors.
type stubChatModel struct {
	err error
}

func (s *stubChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	return nil, s.err
}
func (s *stubChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, s.err
}
func (s *stubChatModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	return s, nil
}
