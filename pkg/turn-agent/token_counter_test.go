package turnagent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// =============================================================================
// EstimateMessageTokensPrecise tests
// =============================================================================

func TestEstimateMessageTokensPrecise_NilMessage(t *testing.T) {
	got := EstimateMessageTokensPrecise(nil)
	if got != 0 {
		t.Errorf("nil message: got %d, want 0", got)
	}
}

func TestEstimateMessageTokensPrecise_SimpleContent(t *testing.T) {
	msg := &schema.Message{
		Role:    schema.User,
		Content: "hello world", // 11 chars
	}
	got := EstimateMessageTokensPrecise(msg)
	// 11 / 4 = 2 (integer division)
	if got != 2 {
		t.Errorf("simple content: got %d, want 2", got)
	}
}

func TestEstimateMessageTokensPrecise_WithReasoningContent(t *testing.T) {
	msg := &schema.Message{
		Role:             schema.Assistant,
		Content:          "answer",           // 6 chars
		ReasoningContent: "thinking process", // 16 chars
	}
	got := EstimateMessageTokensPrecise(msg)
	// (6 + 16) / 4 = 22 / 4 = 5
	if got != 5 {
		t.Errorf("with reasoning: got %d, want 5", got)
	}
}

func TestEstimateMessageTokensPrecise_MultimodalText(t *testing.T) {
	msg := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "describe this"},
		},
	}
	got := EstimateMessageTokensPrecise(msg)
	// 13 / 4 = 3
	if got != 3 {
		t.Errorf("multimodal text: got %d, want 3", got)
	}
}

func TestEstimateMessageTokensPrecise_MultimodalImage(t *testing.T) {
	msg := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "hello"}, // 5 chars
			{Type: schema.ChatMessagePartTypeImageURL},            // 1000 chars (estimated)
		},
	}
	got := EstimateMessageTokensPrecise(msg)
	// (5 + 1000) / 4 = 1005 / 4 = 251
	if got != 251 {
		t.Errorf("multimodal image: got %d, want 251", got)
	}
}

func TestEstimateMessageTokensPrecise_AssistantGenMultiContent(t *testing.T) {
	msg := &schema.Message{
		Role: schema.Assistant,
		AssistantGenMultiContent: []schema.MessageOutputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "output"},                                                // 6 chars
			{Type: schema.ChatMessagePartTypeReasoning, Reasoning: &schema.MessageOutputReasoning{Text: "think"}}, // 5 chars
		},
	}
	got := EstimateMessageTokensPrecise(msg)
	// (6 + 5) / 4 = 11 / 4 = 2
	if got != 2 {
		t.Errorf("assistant gen multi content: got %d, want 2", got)
	}
}

func TestEstimateMessageTokensPrecise_ToolCalls(t *testing.T) {
	msg := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{
				ID: "call_123", // 8 chars
				Function: schema.FunctionCall{
					Name:      "search",     // 6 chars
					Arguments: `{"q":"go"}`, // 11 chars
				},
			},
		},
	}
	got := EstimateMessageTokensPrecise(msg)
	// (8 + 6 + 11) / 4 = 25 / 4 = 6
	if got != 6 {
		t.Errorf("tool calls: got %d, want 6", got)
	}
}

func TestEstimateMessageTokensPrecise_ToolResult(t *testing.T) {
	msg := &schema.Message{
		Role:       schema.Tool,
		Content:    "result data", // 11 chars
		ToolCallID: "call_abc",    // 8 chars
		ToolName:   "search",      // 6 chars
	}
	got := EstimateMessageTokensPrecise(msg)
	// (11 + 8 + 6) / 4 = 25 / 4 = 6
	if got != 6 {
		t.Errorf("tool result: got %d, want 6", got)
	}
}

// =============================================================================
// CumulativeTokenCounter tests
// =============================================================================

func TestCumulativeTokenCounter_EmptyMessages(t *testing.T) {
	got, err := CumulativeTokenCounter(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("empty messages: got %d, want 0", got)
	}
}

func TestCumulativeTokenCounter_NoUsageData(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hello"},      // 5 chars -> 1 token
		{Role: schema.Assistant, Content: "world"}, // 5 chars -> 1 token
	}
	got, err := CumulativeTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1 + 1 = 2
	if got != 2 {
		t.Errorf("no usage data: got %d, want 2", got)
	}
}

func TestCumulativeTokenCounter_WithAssistantUsage(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "question"},
		{Role: schema.Assistant, Content: "answer", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{TotalTokens: 100},
		}},
		{Role: schema.User, Content: "follow up"}, // 9 chars -> 2 tokens
	}
	got, err := CumulativeTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// baseline: 100, then 2 estimated
	if got != 102 {
		t.Errorf("with assistant usage: got %d, want 102", got)
	}
}

func TestCumulativeTokenCounter_AnyRoleUsage(t *testing.T) {
	// The key fix: CumulativeTokenCounter should find Usage on ANY role, not just Assistant.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Tool, Content: "result", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{TotalTokens: 200},
		}},
		{Role: schema.User, Content: "next question"}, // 13 chars -> 3 tokens
	}
	got, err := CumulativeTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// baseline: 200, then 3 estimated
	if got != 203 {
		t.Errorf("any role usage: got %d, want 203", got)
	}
}

func TestCumulativeTokenCounter_TotalTokensZeroProtection(t *testing.T) {
	// TotalTokens == 0 should be skipped (protection against invalid data).
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: "old", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{TotalTokens: 0}, // should be skipped
		}},
		{Role: schema.User, Content: "hello world"}, // 11 chars -> 2 tokens
		{Role: schema.Assistant, Content: "hi"},     // 2 chars -> 0 tokens
	}
	got, err := CumulativeTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No valid baseline, so all messages estimated:
	// "old" (3 chars) -> 0, "hello world" (11 chars) -> 2, "hi" (2 chars) -> 0 = total 2
	if got != 2 {
		t.Errorf("TotalTokens==0 protection: got %d, want 2", got)
	}
}

func TestCumulativeTokenCounter_UsesLastestUsage(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: "first", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{TotalTokens: 50},
		}},
		{Role: schema.User, Content: "second"},
		{Role: schema.Assistant, Content: "third", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{TotalTokens: 150},
		}},
		{Role: schema.User, Content: "fourth"}, // 6 chars -> 1 token
	}
	got, err := CumulativeTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Uses the last Usage (150), then adds 1 for "fourth"
	if got != 151 {
		t.Errorf("uses latest usage: got %d, want 151", got)
	}
}

// =============================================================================
// Consistency test: both wrappers should produce identical results
// =============================================================================

func TestConsistency_SharedAndWrapperMatch(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "world", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{TotalTokens: 100},
		}},
		{
			Role: schema.User,
			UserInputMultiContent: []schema.MessageInputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "look at this"},
				{Type: schema.ChatMessagePartTypeImageURL},
			},
		},
		{Role: schema.Assistant, Content: "nice image", ToolCalls: []schema.ToolCall{
			{ID: "tc1", Function: schema.FunctionCall{Name: "analyze", Arguments: `{"size":"large"}`}},
		}},
		{Role: schema.Tool, Content: "analysis result", ToolCallID: "tc1", ToolName: "analyze"},
	}

	// Direct call
	direct, err := CumulativeTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("CumulativeTokenCounter error: %v", err)
	}

	// Via defaultTokenCounter (in summarize_middleware.go)
	viaDefault, err := defaultTokenCounter(context.Background(), msgs)
	if err != nil {
		t.Fatalf("defaultTokenCounter error: %v", err)
	}

	if direct != viaDefault {
		t.Errorf("consistency: CumulativeTokenCounter=%d, defaultTokenCounter=%d, want equal", direct, viaDefault)
	}

	// Also check per-message consistency
	for i, msg := range msgs {
		precise := EstimateMessageTokensPrecise(msg)
		estimate := estimateMessageTokens(msg)
		if precise != estimate {
			t.Errorf("message %d: EstimateMessageTokensPrecise=%d, estimateMessageTokens=%d, want equal", i, precise, estimate)
		}
	}
}
