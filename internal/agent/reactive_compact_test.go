package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// aggressiveMicrocompact tests
// =============================================================================

// helper to create a tool call assistant message followed by its tool result.
func toolCallPair(toolName, callID, resultContent string) []*turnagent.Message {
	return []*turnagent.Message{
		{
			Role:      turnagent.RoleAssistant,
			Content:   "",
			ToolCalls: []turnagent.ToolCall{{ID: callID, Name: toolName}},
		},
		{
			Role:       turnagent.RoleTool,
			Content:    resultContent,
			ToolName:   toolName,
			ToolCallID: callID,
		},
	}
}

func TestAggressiveMicrocompact_EmptyMessages(t *testing.T) {
	result := aggressiveMicrocompact(nil, 1)
	if len(result) != 0 {
		t.Errorf("expected empty result for nil input, got %d messages", len(result))
	}

	result = aggressiveMicrocompact([]*turnagent.Message{}, 1)
	if len(result) != 0 {
		t.Errorf("expected empty result for empty input, got %d messages", len(result))
	}
}

func TestAggressiveMicrocompact_NoCompactableTools(t *testing.T) {
	// ask_user is NOT in CompactableTools, so its result should not be cleared
	messages := []*turnagent.Message{
		{Role: turnagent.RoleUser, Content: "hello"},
		{
			Role:      turnagent.RoleAssistant,
			Content:   "",
			ToolCalls: []turnagent.ToolCall{{ID: "c1", Name: "ask_user"}},
		},
		{Role: turnagent.RoleTool, Content: "user answer", ToolName: "ask_user", ToolCallID: "c1"},
	}

	result := aggressiveMicrocompact(messages, 0)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	// ask_user tool result should NOT be cleared
	if result[2].Content != "user answer" {
		t.Errorf("ask_user tool result should not be cleared, got %q", result[2].Content)
	}
}

func TestAggressiveMicrocompact_KeepRecent(t *testing.T) {
	// 3 compactable tool call/result pairs, keep 1 (the last)
	var messages []*turnagent.Message
	messages = append(messages, &turnagent.Message{Role: turnagent.RoleUser, Content: "start"})
	messages = append(messages, toolCallPair("read", "c1", "file content 1")...)
	messages = append(messages, toolCallPair("read", "c2", "file content 2")...)
	messages = append(messages, toolCallPair("grep", "c3", "grep result 3")...)

	result := aggressiveMicrocompact(messages, 1)

	// 1 user + 3*(assistant+tool) = 7 messages total
	if len(result) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(result))
	}

	// First two tool results (index 2, 4) should be cleared
	if result[2].Content != TimeBasedMCClearedMessage {
		t.Errorf("expected first tool result cleared, got %q", result[2].Content)
	}
	if result[4].Content != TimeBasedMCClearedMessage {
		t.Errorf("expected second tool result cleared, got %q", result[4].Content)
	}
	// Last tool result (index 6) should be preserved
	if result[6].Content != "grep result 3" {
		t.Errorf("expected last tool result preserved, got %q", result[6].Content)
	}
}

func TestAggressiveMicrocompact_KeepZero(t *testing.T) {
	// keepRecent=0 should clear ALL compactable tool results
	var messages []*turnagent.Message
	messages = append(messages, toolCallPair("read", "c1", "result 1")...)
	messages = append(messages, toolCallPair("write", "c2", "result 2")...)

	result := aggressiveMicrocompact(messages, 0)
	// 2*(assistant+tool) = 4 messages
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
	// Tool results at index 1 and 3 should be cleared
	if result[1].Content != TimeBasedMCClearedMessage {
		t.Errorf("message[1] should be cleared, got %q", result[1].Content)
	}
	if result[3].Content != TimeBasedMCClearedMessage {
		t.Errorf("message[3] should be cleared, got %q", result[3].Content)
	}
	// Assistant messages (with tool calls) should not be modified
	if len(result[0].ToolCalls) == 0 {
		t.Error("assistant tool calls should be preserved")
	}
}

func TestAggressiveMicrocompact_KeepAll(t *testing.T) {
	// keepRecent >= total compactable count → no clearing
	messages := toolCallPair("read", "c1", "result 1")

	result := aggressiveMicrocompact(messages, 5)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[1].Content != "result 1" {
		t.Errorf("tool result should be preserved, got %q", result[1].Content)
	}
}

func TestAggressiveMicrocompact_PreservesMetadata(t *testing.T) {
	messages := []*turnagent.Message{
		{
			Role:      turnagent.RoleAssistant,
			ToolCalls: []turnagent.ToolCall{{ID: "call_abc", Name: "read"}},
		},
		{
			Role:       turnagent.RoleTool,
			Content:    "original content",
			ToolName:   "read",
			ToolCallID: "call_abc",
		},
	}

	result := aggressiveMicrocompact(messages, 0)
	if result[1].ToolName != "read" {
		t.Errorf("ToolName should be preserved, got %q", result[1].ToolName)
	}
	if result[1].ToolCallID != "call_abc" {
		t.Errorf("ToolCallID should be preserved, got %q", result[1].ToolCallID)
	}
	if result[1].Content != TimeBasedMCClearedMessage {
		t.Errorf("Content should be cleared message, got %q", result[1].Content)
	}
}

func TestAggressiveMicrocompact_MixedCompactableAndNonCompactable(t *testing.T) {
	// Mix of compactable and non-compactable tool calls
	messages := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: turnagent.RoleTool, Content: "read result", ToolName: "read", ToolCallID: "c1"},
		{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c2", Name: "ask_user"}}},
		{Role: turnagent.RoleTool, Content: "ask result", ToolName: "ask_user", ToolCallID: "c2"},
		{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c3", Name: "write"}}},
		{Role: turnagent.RoleTool, Content: "write result", ToolName: "write", ToolCallID: "c3"},
		{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{{ID: "c4", Name: "script"}}},
		{Role: turnagent.RoleTool, Content: "script result", ToolName: "script", ToolCallID: "c4"},
	}

	result := aggressiveMicrocompact(messages, 0)

	// read (c1), write (c3), script (c4) should be cleared; ask_user (c2) preserved
	if result[1].Content != TimeBasedMCClearedMessage {
		t.Errorf("read result should be cleared, got %q", result[1].Content)
	}
	if result[3].Content != "ask result" {
		t.Errorf("ask_user result should be preserved, got %q", result[3].Content)
	}
	if result[5].Content != TimeBasedMCClearedMessage {
		t.Errorf("write result should be cleared, got %q", result[5].Content)
	}
	if result[7].Content != TimeBasedMCClearedMessage {
		t.Errorf("script result should be cleared, got %q", result[7].Content)
	}
}

// =============================================================================
// Retention config tests
// =============================================================================

func TestAggressiveRetentionConfig(t *testing.T) {
	cfg := aggressiveRetentionConfig()
	if cfg.MinTokens != 2000 {
		t.Errorf("expected MinTokens=2000, got %d", cfg.MinTokens)
	}
	if cfg.MinTextBlockMessages != 2 {
		t.Errorf("expected MinTextBlockMessages=2, got %d", cfg.MinTextBlockMessages)
	}
	if cfg.MaxTokens != 10000 {
		t.Errorf("expected MaxTokens=10000, got %d", cfg.MaxTokens)
	}
}

func TestMinimalRetentionConfig(t *testing.T) {
	cfg := minimalRetentionConfig()
	if cfg.MinTokens != 500 {
		t.Errorf("expected MinTokens=500, got %d", cfg.MinTokens)
	}
	if cfg.MinTextBlockMessages != 1 {
		t.Errorf("expected MinTextBlockMessages=1, got %d", cfg.MinTextBlockMessages)
	}
	if cfg.MaxTokens != 5000 {
		t.Errorf("expected MaxTokens=5000, got %d", cfg.MaxTokens)
	}
}

// =============================================================================
// buildCompressedResult tests
// =============================================================================

func TestBuildCompressedResult_PreservesSystemMessages(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.System, Content: "system instruction"},
		{Role: schema.User, Content: "old message 1"},
		{Role: schema.Assistant, Content: "old response 1"},
		{Role: schema.User, Content: "recent message"},
	}

	// retentionIndex=2: first 2 messages compressed, last 2 retained
	result := buildCompressedResult(msgs, 2, "this is the summary")

	// Expected: summary + system message + 2 retained messages = 4
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
	if result[0].Role != schema.User {
		t.Errorf("first message should be summary (user role), got %s", result[0].Role)
	}
	if result[1].Role != schema.System {
		t.Errorf("second message should be system, got %s", result[1].Role)
	}
	if result[1].Content != "system instruction" {
		t.Errorf("system message should be preserved, got %q", result[1].Content)
	}
}

func TestBuildCompressedResult_PartialCompact(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "msg 1"},
		{Role: schema.Assistant, Content: "msg 2"},
		{Role: schema.User, Content: "msg 3"},
	}

	// retentionIndex=1: first 1 message compressed into summary, last 2 retained
	result := buildCompressedResult(msgs, 1, "summary text")

	// Expected: summary + retained msg 2,3 = 3 messages
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	// Summary is first
	if result[0].Role != schema.User {
		t.Errorf("first should be summary (user), got %s", result[0].Role)
	}
	// Retained messages follow
	if result[1].Content != "msg 2" {
		t.Errorf("retained message should be 'msg 2', got %q", result[1].Content)
	}
	if result[2].Content != "msg 3" {
		t.Errorf("retained message should be 'msg 3', got %q", result[2].Content)
	}
}

func TestBuildCompressedResult_NoSystemMessagesInDiscarded(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "msg 1"},
		{Role: schema.Assistant, Content: "msg 2"},
	}

	// retentionIndex=2: all messages discarded (compressed), no retained
	result := buildCompressedResult(msgs, 2, "summary")

	// Expected: just the summary message
	if len(result) != 1 {
		t.Fatalf("expected 1 message (just summary), got %d", len(result))
	}
	if result[0].Role != schema.User {
		t.Errorf("summary should be user role, got %s", result[0].Role)
	}
	if !strings.Contains(result[0].Content, "summary") {
		t.Errorf("summary content should contain 'summary', got %q", result[0].Content)
	}
}

func TestBuildCompressedResult_EmptyRetention(t *testing.T) {
	// retentionIndex == len(msgs) means nothing to retain
	msgs := []*schema.Message{
		{Role: schema.User, Content: "msg 1"},
	}

	result := buildCompressedResult(msgs, 1, "summary")
	if len(result) != 1 {
		t.Fatalf("expected 1 message (just summary), got %d", len(result))
	}
}

// =============================================================================
// estimateTokensAfterCompact tests
// =============================================================================

func TestEstimateTokensAfterCompact(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("a", 4000)},      // ~1000 tokens
		{Role: schema.Assistant, Content: strings.Repeat("b", 8000)}, // ~2000 tokens
	}

	// retentionIndex=1: first message compressed, second retained
	tokens := estimateTokensAfterCompact(msgs, 1, "summary text")

	// summary ~3 tokens + retained msg ~2000 tokens = ~2003
	if tokens < 1900 || tokens > 2100 {
		t.Errorf("expected ~2003 tokens, got %d", tokens)
	}
}

func TestEstimateTokensAfterCompact_NoRetained(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "old"},
	}

	tokens := estimateTokensAfterCompact(msgs, 1, "summary")
	// Only summary tokens (4 chars / 4 = 1 token approximately)
	if tokens < 1 || tokens > 10 {
		t.Errorf("expected ~1-10 tokens for just summary, got %d", tokens)
	}
}
