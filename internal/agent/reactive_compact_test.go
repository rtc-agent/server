package agent

import (
	"testing"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// newAssistantWithToolCalls creates an assistant message with tool calls.
func newAssistantWithToolCalls(toolCalls ...turnagent.ToolCall) *turnagent.Message {
	return &turnagent.Message{
		Role:      turnagent.RoleAssistant,
		ToolCalls: toolCalls,
		CreatedAt: time.Now(),
	}
}

// newToolResult creates a tool result message.
func newToolResultMsg(toolCallID, toolName, content string) *turnagent.Message {
	return &turnagent.Message{
		Role:       turnagent.RoleTool,
		ToolCallID: toolCallID,
		ToolName:   toolName,
		Content:    content,
		CreatedAt:  time.Now(),
	}
}

func TestAggressiveMicrocompact_EmptyMessages(t *testing.T) {
	result := aggressiveMicrocompact(nil, 1)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d messages", len(result))
	}
}

func TestAggressiveMicrocompact_NoCompactableTools(t *testing.T) {
	// Messages with no compactable tool calls should be returned unchanged.
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleUser, Content: "hello"},
		{Role: turnagent.RoleAssistant, Content: "hi there"},
		{Role: turnagent.RoleUser, Content: "how are you?"},
	}
	result := aggressiveMicrocompact(msgs, 1)
	if len(result) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(result))
	}
	for i, msg := range result {
		if msg.Content != msgs[i].Content {
			t.Errorf("message %d content changed: got %q, want %q", i, msg.Content, msgs[i].Content)
		}
	}
}

func TestAggressiveMicrocompact_KeepRecent1(t *testing.T) {
	// 3 tool results, keep 1 → oldest 2 should be cleared.
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleUser, Content: "read files"},
		newAssistantWithToolCalls(
			turnagent.ToolCall{ID: "tc1", Name: "read"},
			turnagent.ToolCall{ID: "tc2", Name: "read"},
			turnagent.ToolCall{ID: "tc3", Name: "read"},
		),
		newToolResultMsg("tc1", "read", "file content 1"),
		newToolResultMsg("tc2", "read", "file content 2"),
		newToolResultMsg("tc3", "read", "file content 3"),
	}

	result := aggressiveMicrocompact(msgs, 1)
	if len(result) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(result))
	}

	// tc1 and tc2 should be cleared, tc3 should be kept.
	if result[2].Content != TimeBasedMCClearedMessage {
		t.Errorf("tc1 should be cleared, got %q", result[2].Content)
	}
	if result[3].Content != TimeBasedMCClearedMessage {
		t.Errorf("tc2 should be cleared, got %q", result[3].Content)
	}
	if result[4].Content != "file content 3" {
		t.Errorf("tc3 should be kept, got %q", result[4].Content)
	}
}

func TestAggressiveMicrocompact_KeepRecent0(t *testing.T) {
	// keepRecent=0 → ALL tool results should be cleared.
	msgs := []*turnagent.Message{
		newAssistantWithToolCalls(turnagent.ToolCall{ID: "tc1", Name: "read"}),
		newToolResultMsg("tc1", "read", "content"),
	}

	result := aggressiveMicrocompact(msgs, 0)
	if result[1].Content != TimeBasedMCClearedMessage {
		t.Errorf("expected cleared message, got %q", result[1].Content)
	}
}

func TestAggressiveMicrocompact_KeepRecentExceedsTotal(t *testing.T) {
	// keepRecent >= total compactable → nothing should be cleared.
	msgs := []*turnagent.Message{
		newAssistantWithToolCalls(turnagent.ToolCall{ID: "tc1", Name: "read"}),
		newToolResultMsg("tc1", "read", "content"),
	}

	result := aggressiveMicrocompact(msgs, 5)
	if result[1].Content != "content" {
		t.Errorf("expected original content, got %q", result[1].Content)
	}
}

func TestAggressiveMicrocompact_NonCompactableToolIgnored(t *testing.T) {
	// Tools not in CompactableTools should not be cleared.
	msgs := []*turnagent.Message{
		newAssistantWithToolCalls(turnagent.ToolCall{ID: "tc1", Name: "ask_user"}),
		newToolResultMsg("tc1", "ask_user", "user answer"),
	}

	result := aggressiveMicrocompact(msgs, 0)
	if result[1].Content != "user answer" {
		t.Errorf("non-compactable tool result should be preserved, got %q", result[1].Content)
	}
}

func TestAggressiveMicrocompact_MixedTools(t *testing.T) {
	// Mix of compactable and non-compactable tools.
	msgs := []*turnagent.Message{
		newAssistantWithToolCalls(
			turnagent.ToolCall{ID: "tc1", Name: "read"},
			turnagent.ToolCall{ID: "tc2", Name: "ask_user"},
			turnagent.ToolCall{ID: "tc3", Name: "write"},
		),
		newToolResultMsg("tc1", "read", "file data"),
		newToolResultMsg("tc2", "ask_user", "user reply"),
		newToolResultMsg("tc3", "write", "write ok"),
	}

	// Keep 1 compactable → tc3 (write) is kept, tc1 (read) is cleared.
	result := aggressiveMicrocompact(msgs, 1)
	if result[1].Content != TimeBasedMCClearedMessage {
		t.Errorf("tc1 (read) should be cleared, got %q", result[1].Content)
	}
	if result[2].Content != "user reply" {
		t.Errorf("tc2 (ask_user) should be preserved, got %q", result[2].Content)
	}
	if result[3].Content != "write ok" {
		t.Errorf("tc3 (write) should be kept (most recent compactable), got %q", result[3].Content)
	}
}

func TestAggressiveMicrocompact_ToolCallIDPreserved(t *testing.T) {
	// Cleared messages should still have the correct ToolCallID.
	msgs := []*turnagent.Message{
		newAssistantWithToolCalls(turnagent.ToolCall{ID: "tc1", Name: "read"}),
		newToolResultMsg("tc1", "read", "content"),
	}

	result := aggressiveMicrocompact(msgs, 0)
	if result[1].ToolCallID != "tc1" {
		t.Errorf("ToolCallID should be preserved after clearing, got %q", result[1].ToolCallID)
	}
	if result[1].ToolName != "read" {
		t.Errorf("ToolName should be preserved after clearing, got %q", result[1].ToolName)
	}
}
