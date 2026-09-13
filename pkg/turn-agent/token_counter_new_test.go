package turnagent

import (
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// =============================================================================
// HeuristicTokenCounter tests
// =============================================================================

func TestHeuristicTokenCounter_CountTokens(t *testing.T) {
	tc := &HeuristicTokenCounter{}

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty", "", 0},
		{"short", "hi", 0},
		{"english", "hello world", 2},   // 11 chars / 4 = 2
		{"longer", "this is a test", 3}, // 14 chars / 4 = 3
		{"chinese", "你好世界", 3},          // 12 bytes / 4 = 3 (UTF-8: 3 bytes per char)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tc.CountTokens(tt.input)
			if got != tt.want {
				t.Errorf("CountTokens(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestHeuristicTokenCounter_CountMessageTokens(t *testing.T) {
	tc := &HeuristicTokenCounter{}

	tests := []struct {
		name string
		msg  *schema.Message
		want int
	}{
		{
			name: "nil message",
			msg:  nil,
			want: 0,
		},
		{
			name: "simple content",
			msg:  &schema.Message{Role: schema.User, Content: "hello world"},
			want: 2, // 11 / 4 = 2
		},
		{
			name: "with reasoning",
			msg: &schema.Message{
				Role:             schema.Assistant,
				Content:          "answer",
				ReasoningContent: "thinking process",
			},
			want: 5, // (6 + 16) / 4 = 5
		},
		{
			name: "with tool calls",
			msg: &schema.Message{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{
					{
						ID: "call_123",
						Function: schema.FunctionCall{
							Name:      "search",
							Arguments: `{"q":"go"}`,
						},
					},
				},
			},
			want: 6, // (8 + 6 + 11) / 4 = 6
		},
		{
			name: "tool result",
			msg: &schema.Message{
				Role:       schema.Tool,
				Content:    "result data",
				ToolCallID: "call_abc",
				ToolName:   "search",
			},
			want: 6, // (11 + 8 + 6) / 4 = 6
		},
		{
			name: "multimodal image",
			msg: &schema.Message{
				Role: schema.User,
				UserInputMultiContent: []schema.MessageInputPart{
					{Type: schema.ChatMessagePartTypeText, Text: "hello"},
					{Type: schema.ChatMessagePartTypeImageURL},
				},
			},
			want: 251, // (5 + 1000) / 4 = 251
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tc.CountMessageTokens(tt.msg)
			if got != tt.want {
				t.Errorf("CountMessageTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// =============================================================================
// TiktokenTokenCounter tests
// =============================================================================

func TestTiktokenTokenCounter_CountTokens(t *testing.T) {
	tc := NewTiktokenTokenCounter()

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty", "", 0},
		{"short", "hi", 1},
		{"english", "hello world", 2},
		{"chinese", "你好世界", 6},   // Chinese characters are more tokens
		{"mixed", "hello 你好", 4}, // Mixed language
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tc.CountTokens(tt.input)
			// Allow ±2 token variance for different encodings
			if got < tt.want-2 || got > tt.want+2 {
				t.Errorf("CountTokens(%q) = %d, want ~%d", tt.input, got, tt.want)
			}
		})
	}
}

func TestTiktokenTokenCounter_CountMessageTokens(t *testing.T) {
	tc := NewTiktokenTokenCounter()

	msg := &schema.Message{
		Role:    schema.User,
		Content: "hello world",
	}

	got := tc.CountMessageTokens(msg)
	if got <= 0 {
		t.Errorf("CountMessageTokens() = %d, want > 0", got)
	}
}

func TestTiktokenTokenCounter_FallbackOnError(t *testing.T) {
	// Create a counter with invalid encoding to test fallback
	tc := &TiktokenTokenCounter{}
	tc.once.Do(func() {
		// Simulate initialization failure
		tc.err = nil // No error, but encoding is nil
	})

	// Should fallback to heuristic
	got := tc.CountTokens("hello world")
	if got != 2 { // 11 / 4 = 2
		t.Errorf("CountTokens() fallback = %d, want 2", got)
	}
}

// =============================================================================
// Global TokenCounter tests
// =============================================================================

func TestGlobalTokenCounter_Default(t *testing.T) {
	// Reset to default
	SetGlobalTokenCounter(&HeuristicTokenCounter{})

	got := CountStringTokens("hello world")
	if got != 2 { // 11 / 4 = 2
		t.Errorf("CountStringTokens() = %d, want 2", got)
	}
}

func TestGlobalTokenCounter_SwitchToTiktoken(t *testing.T) {
	// Save original
	original := GetGlobalTokenCounter()
	defer SetGlobalTokenCounter(original)

	// Switch to tiktoken
	SetGlobalTokenCounter(NewTiktokenTokenCounter())

	got := CountStringTokens("hello world")
	// Tiktoken should give similar result (±2)
	if got < 0 || got > 4 {
		t.Errorf("CountStringTokens() = %d, want 0-4", got)
	}
}

func TestSetGlobalTokenCounter_Nil(t *testing.T) {
	// Save original
	original := GetGlobalTokenCounter()
	defer SetGlobalTokenCounter(original)

	// Setting nil should not change the counter
	SetGlobalTokenCounter(nil)
	if GetGlobalTokenCounter() != original {
		t.Error("SetGlobalTokenCounter(nil) should not change the counter")
	}
}

// =============================================================================
// NewTokenCounter tests
// =============================================================================

func TestNewTokenCounter_Heuristic(t *testing.T) {
	tc := NewTokenCounter("heuristic")
	if _, ok := tc.(*HeuristicTokenCounter); !ok {
		t.Errorf("NewTokenCounter(heuristic) should return *HeuristicTokenCounter")
	}
}

func TestNewTokenCounter_Tokenizer(t *testing.T) {
	tc := NewTokenCounter("tokenizer")
	if _, ok := tc.(*TiktokenTokenCounter); !ok {
		t.Errorf("NewTokenCounter(tokenizer) should return *TiktokenTokenCounter")
	}
}

func TestNewTokenCounter_Default(t *testing.T) {
	tc := NewTokenCounter("unknown")
	if _, ok := tc.(*HeuristicTokenCounter); !ok {
		t.Errorf("NewTokenCounter(unknown) should return *HeuristicTokenCounter")
	}
}

func TestNewTokenCounter_CaseInsensitive(t *testing.T) {
	tc := NewTokenCounter("TOKENIZER")
	if _, ok := tc.(*TiktokenTokenCounter); !ok {
		t.Errorf("NewTokenCounter(TOKENIZER) should return *TiktokenTokenCounter")
	}
}

// =============================================================================
// Concurrent safety tests
// =============================================================================

func TestTiktokenTokenCounter_ConcurrentSafety(t *testing.T) {
	tc := NewTiktokenTokenCounter()
	text := strings.Repeat("hello world ", 100)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = tc.CountTokens(text)
			}
		}()
	}
	wg.Wait()
}

func TestGlobalTokenCounter_ConcurrentSafety(t *testing.T) {
	// Save original
	original := GetGlobalTokenCounter()
	defer SetGlobalTokenCounter(original)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = CountStringTokens("hello world")
			}
		}()
	}
	wg.Wait()
}

// =============================================================================
// Benchmark tests
// =============================================================================

func BenchmarkHeuristicTokenCounter_CountTokens(b *testing.B) {
	tc := &HeuristicTokenCounter{}
	text := strings.Repeat("hello world ", 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tc.CountTokens(text)
	}
}

func BenchmarkTiktokenTokenCounter_CountTokens(b *testing.B) {
	tc := NewTiktokenTokenCounter()
	text := strings.Repeat("hello world ", 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tc.CountTokens(text)
	}
}

func BenchmarkHeuristicTokenCounter_CountMessageTokens(b *testing.B) {
	tc := &HeuristicTokenCounter{}
	msg := &schema.Message{
		Role:    schema.User,
		Content: strings.Repeat("hello world ", 50),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tc.CountMessageTokens(msg)
	}
}

func BenchmarkTiktokenTokenCounter_CountMessageTokens(b *testing.B) {
	tc := NewTiktokenTokenCounter()
	msg := &schema.Message{
		Role:    schema.User,
		Content: strings.Repeat("hello world ", 50),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tc.CountMessageTokens(msg)
	}
}

// =============================================================================
// Accuracy comparison test
// =============================================================================

func TestAccuracyComparison_HeuristicVsTiktoken(t *testing.T) {
	heuristic := &HeuristicTokenCounter{}
	tiktoken := NewTiktokenTokenCounter()

	testCases := []struct {
		name  string
		input string
	}{
		{"english", "The quick brown fox jumps over the lazy dog"},
		{"chinese", "这是一个中文测试句子，用来验证 token 计数的准确性"},
		{"mixed", "Hello 世界, this is a mixed language test"},
		{"code", "func main() { fmt.Println(\"hello\") }"},
		{"json", `{"name":"test","value":123,"nested":{"key":"value"}}`},
	}

	t.Logf("Accuracy comparison (Heuristic vs Tiktoken):")
	t.Logf("%-10s | %-10s | %-10s | %-10s", "Type", "Heuristic", "Tiktoken", "Diff%")
	t.Logf("%s", strings.Repeat("-", 50))

	for _, tc := range testCases {
		h := heuristic.CountTokens(tc.input)
		tk := tiktoken.CountTokens(tc.input)
		diff := float64(h-tk) / float64(tk) * 100
		t.Logf("%-10s | %-10d | %-10d | %+6.1f%%", tc.name, h, tk, diff)
	}
}
