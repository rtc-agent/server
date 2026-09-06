package agent

import (
	"strings"
	"testing"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestApplyToolResultBudget_EmptyMessages(t *testing.T) {
	msgs := []*turnagent.Message{}
	result := applyToolResultBudget(msgs, DefaultToolResultBudgetConfig())
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d messages", len(result))
	}
}

func TestApplyToolResultBudget_NoToolMessages(t *testing.T) {
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleUser, Content: "Hello"},
		{Role: turnagent.RoleAssistant, Content: "Hi there"},
	}
	result := applyToolResultBudget(msgs, DefaultToolResultBudgetConfig())
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
	// Verify content is unchanged
	if result[0].Content != "Hello" || result[1].Content != "Hi there" {
		t.Error("non-tool messages should not be modified")
	}
}

func TestApplyToolResultBudget_SmallToolResult(t *testing.T) {
	// Create a tool result under the limit (10000 tokens = 40000 chars)
	smallContent := strings.Repeat("x", 30000) // 30KB, under 40KB limit
	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    smallContent,
			ToolName:   "read",
			ToolCallID: "tool-1",
			CreatedAt:  time.Now(),
		},
	}

	result := applyToolResultBudget(msgs, DefaultToolResultBudgetConfig())
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	// Should not be truncated
	if result[0].Content != smallContent {
		t.Error("small tool result should not be truncated")
	}
}

func TestApplyToolResultBudget_LargeToolResult_Truncated(t *testing.T) {
	// Create a tool result over the limit (10000 tokens = 40000 chars)
	// Use 50000 chars to ensure it's over the limit
	largeContent := strings.Repeat("x", 50000)
	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    largeContent,
			ToolName:   "read",
			ToolCallID: "tool-1",
			CreatedAt:  time.Now(),
		},
	}

	cfg := ToolResultBudgetConfig{MaxTokens: 10000} // 40000 chars
	result := applyToolResultBudget(msgs, cfg)

	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}

	// Should be truncated
	if result[0].Content == largeContent {
		t.Error("large tool result should be truncated")
	}

	// Should contain truncation marker
	if !strings.Contains(result[0].Content, ToolResultTruncateMsg) {
		t.Error("truncated content should contain truncation marker")
	}

	// Verify head (60%) + tail (20%) preservation
	maxChars := 10000 * 4 // 40000
	expectedHeadChars := int(float64(maxChars) * 0.6)
	expectedTailChars := int(float64(maxChars) * 0.2)

	head := result[0].Content[:expectedHeadChars]
	if !strings.HasPrefix(head, strings.Repeat("x", expectedHeadChars)) {
		t.Error("head should be preserved correctly")
	}

	// Check that tail is from the end of original content
	tailStart := len(result[0].Content) - expectedTailChars
	tail := result[0].Content[tailStart:]
	if !strings.HasPrefix(tail, strings.Repeat("x", expectedTailChars)) {
		t.Error("tail should be preserved correctly")
	}

	// Original message should not be modified
	if msgs[0].Content != largeContent {
		t.Error("original message should not be modified")
	}
}

func TestApplyToolResultBudget_MultipleToolResults(t *testing.T) {
	smallContent := strings.Repeat("a", 10000)  // 10KB
	largeContent := strings.Repeat("b", 50000) // 50KB, over limit

	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    smallContent,
			ToolName:   "read",
			ToolCallID: "tool-1",
		},
		{
			Role:       turnagent.RoleTool,
			Content:    largeContent,
			ToolName:   "grep",
			ToolCallID: "tool-2",
		},
		{
			Role:       turnagent.RoleUser,
			Content:    "Some user message",
		},
	}

	cfg := ToolResultBudgetConfig{MaxTokens: 10000} // 40000 chars
	result := applyToolResultBudget(msgs, cfg)

	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	// First tool result should not be truncated
	if result[0].Content != smallContent {
		t.Error("small tool result should not be truncated")
	}

	// Second tool result should be truncated
	if !strings.Contains(result[1].Content, ToolResultTruncateMsg) {
		t.Error("large tool result should be truncated")
	}

	// User message should be unchanged
	if result[2].Content != "Some user message" {
		t.Error("user message should not be modified")
	}
}

func TestApplyToolResultBudget_ZeroConfig_UsesDefault(t *testing.T) {
	largeContent := strings.Repeat("x", 50000)
	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    largeContent,
			ToolName:   "read",
			ToolCallID: "tool-1",
		},
	}

	// Zero config should use default (10000 tokens)
	cfg := ToolResultBudgetConfig{MaxTokens: 0}
	result := applyToolResultBudget(msgs, cfg)

	// Should be truncated (50000 > 40000)
	if !strings.Contains(result[0].Content, ToolResultTruncateMsg) {
		t.Error("should use default config and truncate")
	}
}

func TestApplyToolResultBudget_PreservesMetadata(t *testing.T) {
	largeContent := strings.Repeat("x", 50000)
	createdAt := time.Now()
	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    largeContent,
			ToolName:   "read",
			ToolCallID: "tool-123",
			CreatedAt:  createdAt,
		},
	}

	cfg := ToolResultBudgetConfig{MaxTokens: 10000}
	result := applyToolResultBudget(msgs, cfg)

	// Verify metadata is preserved
	if result[0].ToolName != "read" {
		t.Error("ToolName should be preserved")
	}
	if result[0].ToolCallID != "tool-123" {
		t.Error("ToolCallID should be preserved")
	}
	if !result[0].CreatedAt.Equal(createdAt) {
		t.Error("CreatedAt should be preserved")
	}
}

func TestApplyToolResultBudget_ExactlyAtLimit(t *testing.T) {
	// Exactly at the limit (40000 chars for 10000 tokens)
	exactContent := strings.Repeat("x", 40000)
	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    exactContent,
			ToolName:   "read",
			ToolCallID: "tool-1",
		},
	}

	cfg := ToolResultBudgetConfig{MaxTokens: 10000}
	result := applyToolResultBudget(msgs, cfg)

	// Should NOT be truncated (not over the limit)
	if result[0].Content != exactContent {
		t.Error("content exactly at limit should not be truncated")
	}
}

func TestApplyToolResultBudget_OneCharOverLimit(t *testing.T) {
	// One character over the limit
	overContent := strings.Repeat("x", 40001)
	msgs := []*turnagent.Message{
		{
			Role:       turnagent.RoleTool,
			Content:    overContent,
			ToolName:   "read",
			ToolCallID: "tool-1",
		},
	}

	cfg := ToolResultBudgetConfig{MaxTokens: 10000}
	result := applyToolResultBudget(msgs, cfg)

	// Should be truncated
	if !strings.Contains(result[0].Content, ToolResultTruncateMsg) {
		t.Error("content over limit should be truncated")
	}
}
