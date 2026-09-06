package agent

import (
	"testing"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestMicrocompactMessages_NoAssistant(t *testing.T) {
	// No assistant message -> no compaction
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleUser, Content: "hello"},
		{Role: turnagent.RoleTool, Content: "tool result", ToolName: "read", ToolCallID: "tc1"},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 0, KeepRecent: 1}
	got := microcompactMessages(msgs, cfg)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[1].Content != "tool result" {
		t.Errorf("tool result should not be cleared without assistant message")
	}
}

func TestMicrocompactMessages_ActiveUser(t *testing.T) {
	// Last assistant message is recent (< gap threshold) -> no compaction
	now := time.Now()
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, Content: "thinking", CreatedAt: now.Add(-5 * time.Minute)},
		{Role: turnagent.RoleTool, Content: "old result", ToolName: "read", ToolCallID: "tc1", CreatedAt: now.Add(-10 * time.Minute)},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 60, KeepRecent: 1}
	got := microcompactMessages(msgs, cfg)
	if got[1].Content != "old result" {
		t.Errorf("expected tool result to be preserved (user is active), got %q", got[1].Content)
	}
}

func TestMicrocompactMessages_IdleUser_ClearsOld(t *testing.T) {
	// Last assistant message is old (>= gap threshold) -> compact old tool results
	now := time.Now()
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, Content: "call1",
			ToolCalls: []turnagent.ToolCall{{ID: "tc1", Name: "read"}},
			CreatedAt: now.Add(-2 * time.Hour)},
		{Role: turnagent.RoleTool, Content: "old result 1", ToolName: "read", ToolCallID: "tc1",
			CreatedAt: now.Add(-2 * time.Hour)},
		{Role: turnagent.RoleAssistant, Content: "call2",
			ToolCalls: []turnagent.ToolCall{{ID: "tc2", Name: "read"}},
			CreatedAt: now.Add(-1 * time.Hour)},
		{Role: turnagent.RoleTool, Content: "old result 2", ToolName: "read", ToolCallID: "tc2",
			CreatedAt: now.Add(-1 * time.Hour)},
		{Role: turnagent.RoleAssistant, Content: "call3",
			ToolCalls: []turnagent.ToolCall{{ID: "tc3", Name: "read"}},
			CreatedAt: now.Add(-90 * time.Minute)},
		{Role: turnagent.RoleTool, Content: "recent result", ToolName: "read", ToolCallID: "tc3",
			CreatedAt: now.Add(-90 * time.Minute)},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 60, KeepRecent: 1}
	got := microcompactMessages(msgs, cfg)

	// tc1 and tc2 should be cleared, tc3 kept
	if got[1].Content != TIME_BASED_MC_CLEARED_MESSAGE {
		t.Errorf("tc1 should be cleared, got %q", got[1].Content)
	}
	if got[3].Content != TIME_BASED_MC_CLEARED_MESSAGE {
		t.Errorf("tc2 should be cleared, got %q", got[3].Content)
	}
	if got[5].Content != "recent result" {
		t.Errorf("tc3 should be kept, got %q", got[5].Content)
	}
}

func TestMicrocompactMessages_NonCompactableTool(t *testing.T) {
	// ls is not in COMPACTABLE_TOOLS -> should not be cleared
	now := time.Now()
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, Content: "call",
			ToolCalls: []turnagent.ToolCall{{ID: "tc1", Name: "ls"}},
			CreatedAt: now.Add(-2 * time.Hour)},
		{Role: turnagent.RoleTool, Content: "dir listing", ToolName: "ls", ToolCallID: "tc1",
			CreatedAt: now.Add(-2 * time.Hour)},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 60, KeepRecent: 1}
	got := microcompactMessages(msgs, cfg)
	if got[1].Content != "dir listing" {
		t.Errorf("ls tool result should not be compacted, got %q", got[1].Content)
	}
}

func TestMicrocompactMessages_KeepRecentClampedToAtLeast1(t *testing.T) {
	// KeepRecent=0 should still keep at least 1
	now := time.Now()
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, Content: "call",
			ToolCalls: []turnagent.ToolCall{{ID: "tc1", Name: "read"}},
			CreatedAt: now.Add(-2 * time.Hour)},
		{Role: turnagent.RoleTool, Content: "result", ToolName: "read", ToolCallID: "tc1",
			CreatedAt: now.Add(-2 * time.Hour)},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 60, KeepRecent: 0}
	got := microcompactMessages(msgs, cfg)
	if got[1].Content != "result" {
		t.Errorf("with 1 tool result and KeepRecent=0, should keep it, got %q", got[1].Content)
	}
}

func TestMicrocompactMessages_DoesNotModifyOriginal(t *testing.T) {
	now := time.Now()
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, Content: "call",
			ToolCalls: []turnagent.ToolCall{{ID: "tc1", Name: "read"}},
			CreatedAt: now.Add(-2 * time.Hour)},
		{Role: turnagent.RoleTool, Content: "original content", ToolName: "read", ToolCallID: "tc1",
			CreatedAt: now.Add(-2 * time.Hour)},
		{Role: turnagent.RoleAssistant, Content: "call2",
			ToolCalls: []turnagent.ToolCall{{ID: "tc2", Name: "read"}},
			CreatedAt: now.Add(-90 * time.Minute)},
		{Role: turnagent.RoleTool, Content: "recent", ToolName: "read", ToolCallID: "tc2",
			CreatedAt: now.Add(-90 * time.Minute)},
	}
	cfg := MicrocompactConfig{GapThresholdMinutes: 60, KeepRecent: 1}
	_ = microcompactMessages(msgs, cfg)
	if msgs[1].Content != "original content" {
		t.Errorf("original slice should not be modified, got %q", msgs[1].Content)
	}
}

func TestCollectCompactableToolCallIDs(t *testing.T) {
	msgs := []*turnagent.Message{
		{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{
			{ID: "tc1", Name: "read"},
			{ID: "tc2", Name: "ls"}, // not compactable
			{ID: "tc3", Name: "grep"},
		}},
		{Role: turnagent.RoleAssistant, ToolCalls: []turnagent.ToolCall{
			{ID: "tc4", Name: "write"},
		}},
	}
	ids := collectCompactableToolCallIDs(msgs)
	expected := []string{"tc1", "tc3", "tc4"}
	if len(ids) != len(expected) {
		t.Fatalf("expected %d IDs, got %d: %v", len(expected), len(ids), ids)
	}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ids[%d] = %q, want %q", i, id, expected[i])
		}
	}
}
