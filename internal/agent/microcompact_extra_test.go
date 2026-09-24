package agent

import (
	"testing"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// TestMicrocompactMessages_NonCompactableMixedWithCompactable verifies that
// non-compactable tool results are NOT cleared even when compactable tool
// results in the same conversation ARE cleared.
// This was a bug: microcompactMessages cleared ALL tool results whose ToolCallID
// was not in the keepSet, without checking if the tool was in CompactableTools.
func TestMicrocompactMessages_NonCompactableMixedWithCompactable(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, Content: "thinking", CreatedAt: old,
			ToolCalls: []turnagent.ToolCall{
				{ID: "tc1", Name: "read"},    // compactable
				{ID: "tc2", Name: "grep"},    // compactable
				{ID: "tc3", Name: "askUser"}, // NOT compactable
			}},
		{Role: turnagent.RoleTool, Content: "read_result", ToolCallID: "tc1", ToolName: "read", CreatedAt: old},
		{Role: turnagent.RoleTool, Content: "grep_result", ToolCallID: "tc2", ToolName: "grep", CreatedAt: old},
		{Role: turnagent.RoleTool, Content: "ask_result", ToolCallID: "tc3", ToolName: "askUser", CreatedAt: old},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 60, KeepRecent: 1}
	result := microcompactMessages(msgs, cfg)

	// tc1 (read) should be cleared — compactable but not the most recent compactable.
	if result[1].Content != MCClearedMessage {
		t.Errorf("tc1 (read) should be cleared, got %q", result[1].Content)
	}
	// tc2 (grep) should be kept — most recent compactable tool.
	if result[2].Content != "grep_result" {
		t.Errorf("tc2 (grep) should be kept, got %q", result[2].Content)
	}
	// tc3 (askUser) should NOT be cleared — not in CompactableTools.
	if result[3].Content != "ask_result" {
		t.Errorf("tc3 (askUser) should NOT be cleared, got %q", result[3].Content)
	}
}

func TestDefaultMicrocompactConfig(t *testing.T) {
	cfg := DefaultMicrocompactConfig()
	if cfg.GapThresholdMinutes != 60 {
		t.Errorf("GapThresholdMinutes = %d, want 60", cfg.GapThresholdMinutes)
	}
	if cfg.KeepRecent != 5 {
		t.Errorf("KeepRecent = %d, want 5", cfg.KeepRecent)
	}
}
