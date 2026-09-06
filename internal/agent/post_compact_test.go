package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// helper to build an assistant message with a read tool call.
func newReadCall(id, path string) *schema.Message {
	args, _ := json.Marshal(postCompactReadArgs{Path: path})
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{
				ID: id,
				Function: schema.FunctionCall{
					Name:      "read",
					Arguments: string(args),
				},
			},
		},
	}
}

// helper to build a tool result message.
func newToolResult(id, content string) *schema.Message {
	return &schema.Message{
		Role:       schema.Tool,
		Content:    content,
		ToolName:   "read",
		ToolCallID: id,
	}
}

func TestPostCompact_NoReads(t *testing.T) {
	discarded := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "hi there"},
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 0 {
		t.Errorf("expected 0 attachments when no reads, got %d", len(got))
	}
}

func TestPostCompact_BasicRecovery(t *testing.T) {
	discarded := []*schema.Message{
		{Role: schema.User, Content: "read foo.go please"},
		newReadCall("tc1", "/workspace/foo.go"),
		newToolResult("tc1", "package main\n\nfunc main() {}"),
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got))
	}
	if got[0].Role != schema.System {
		t.Errorf("expected System role, got %v", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "/workspace/foo.go") {
		t.Errorf("attachment should contain file path, got: %s", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "package main") {
		t.Errorf("attachment should contain file content, got: %s", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "<system-reminder>") {
		t.Errorf("attachment should be wrapped in system-reminder, got: %s", got[0].Content)
	}
}

func TestPostCompact_DedupByPath(t *testing.T) {
	// Same path read twice — only the most recent read's content should appear.
	discarded := []*schema.Message{
		newReadCall("tc1", "/workspace/foo.go"),
		newToolResult("tc1", "v1 content"),
		newReadCall("tc2", "/workspace/foo.go"),
		newToolResult("tc2", "v2 content"),
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment after dedup, got %d", len(got))
	}
	if !strings.Contains(got[0].Content, "v2 content") {
		t.Errorf("should keep most recent read's content, got: %s", got[0].Content)
	}
	if strings.Contains(got[0].Content, "v1 content") {
		t.Errorf("should not contain older read's content, got: %s", got[0].Content)
	}
}

func TestPostCompact_SkipsClearedContent(t *testing.T) {
	discarded := []*schema.Message{
		newReadCall("tc1", "/workspace/foo.go"),
		newToolResult("tc1", TIME_BASED_MC_CLEARED_MESSAGE),
		newReadCall("tc2", "/workspace/bar.go"),
		newToolResult("tc2", "bar content"),
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment (cleared one skipped), got %d", len(got))
	}
	if !strings.Contains(got[0].Content, "/workspace/bar.go") {
		t.Errorf("should recover bar.go, got: %s", got[0].Content)
	}
}

func TestPostCompact_SkipsEmptyContent(t *testing.T) {
	discarded := []*schema.Message{
		newReadCall("tc1", "/workspace/foo.go"),
		newToolResult("tc1", ""),
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 0 {
		t.Errorf("expected 0 attachments for empty content, got %d", len(got))
	}
}

func TestPostCompact_MaxFilesCap(t *testing.T) {
	// 10 different files, but max is 5.
	var discarded []*schema.Message
	for i := 0; i < 10; i++ {
		id := strings.Repeat("x", i+1) // unique IDs
		discarded = append(discarded, newReadCall(id, "/workspace/file"+id+".go"))
		discarded = append(discarded, newToolResult(id, "content of "+id))
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != postCompactMaxFiles {
		t.Errorf("expected %d attachments, got %d", postCompactMaxFiles, len(got))
	}
}

func TestPostCompact_TruncatesLargeFile(t *testing.T) {
	// Content far exceeding per-file budget (2000 tokens * 4 = 8000 chars).
	bigContent := strings.Repeat("a", 20000)
	discarded := []*schema.Message{
		newReadCall("tc1", "/workspace/big.go"),
		newToolResult("tc1", bigContent),
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got))
	}
	if len(got[0].Content) >= len(bigContent) {
		t.Errorf("expected truncation, content length %d >= original %d",
			len(got[0].Content), len(bigContent))
	}
	if !strings.Contains(got[0].Content, "[Content truncated]") {
		t.Errorf("truncated content should contain marker, got: %s", got[0].Content)
	}
}

func TestPostCompact_TotalBudgetStopsEarly(t *testing.T) {
	// Each file ~3000 chars (~750 tokens). Total budget 10000 tokens.
	// After ~13 files, total budget should be hit. But maxFiles=5 wins first.
	var discarded []*schema.Message
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		// ~3000 chars each → ~750 tokens
		content := strings.Repeat("c", 3000)
		discarded = append(discarded, newReadCall(id, "/workspace/f"+id+".go"))
		discarded = append(discarded, newToolResult(id, content))
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) >= 8 {
		t.Errorf("expected total budget or max files to cap attachments, got %d", len(got))
	}
	if len(got) > postCompactMaxFiles {
		t.Errorf("must not exceed maxFiles=%d, got %d", postCompactMaxFiles, len(got))
	}
}

func TestPostCompact_IgnoresNonReadTools(t *testing.T) {
	// grep / write tool calls should not produce attachments.
	discarded := []*schema.Message{
		{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{
				{ID: "tc1", Function: schema.FunctionCall{Name: "grep", Arguments: `{"pattern":"foo"}`}},
			},
		},
		{Role: schema.Tool, Content: "grep result", ToolName: "grep", ToolCallID: "tc1"},
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 0 {
		t.Errorf("expected 0 attachments for non-read tools, got %d", len(got))
	}
}

func TestPostCompact_RecoversMostRecentFiles(t *testing.T) {
	// Files read earlier should appear later in attachments (since we iterate
	// from most recent and append).
	discarded := []*schema.Message{
		newReadCall("tc1", "/workspace/first.go"),
		newToolResult("tc1", "first content"),
		newReadCall("tc2", "/workspace/second.go"),
		newToolResult("tc2", "second content"),
		newReadCall("tc3", "/workspace/third.go"),
		newToolResult("tc3", "third content"),
	}
	h := &helpers{}
	got := h.createPostCompactAttachments(context.Background(), discarded)
	if len(got) != 3 {
		t.Fatalf("expected 3 attachments, got %d", len(got))
	}
	// Most recent (third.go) should come first.
	if !strings.Contains(got[0].Content, "third.go") {
		t.Errorf("first attachment should be most recent (third.go), got: %s", got[0].Content)
	}
	if !strings.Contains(got[2].Content, "first.go") {
		t.Errorf("last attachment should be oldest (first.go), got: %s", got[2].Content)
	}
}

func TestTruncateForPostCompact(t *testing.T) {
	// Small content not truncated.
	small := "hello"
	if got := truncateForPostCompact(small, 10); got != small {
		t.Errorf("small content should not be truncated, got %q", got)
	}

	// Large content truncated with marker.
	large := strings.Repeat("a", 10000)
	truncated := truncateForPostCompact(large, 100) // 400 chars max
	if !strings.Contains(truncated, "[Content truncated]") {
		t.Errorf("expected truncation marker, got: %s", truncated)
	}
	if len(truncated) >= len(large) {
		t.Errorf("truncated should be shorter than original")
	}
}

func TestAppendPostCompactAttachments(t *testing.T) {
	compressed := []*schema.Message{
		{Role: schema.User, Content: "summary"},
		{Role: schema.Assistant, Content: "continuing"},
	}
	discarded := []*schema.Message{
		newReadCall("tc1", "/workspace/foo.go"),
		newToolResult("tc1", "foo content"),
	}
	h := &helpers{}
	got := appendPostCompactAttachments(context.Background(), h, compressed, discarded)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages (compressed + 1 attachment), got %d", len(got))
	}
	if got[0].Content != "summary" {
		t.Errorf("first message should be preserved summary, got %q", got[0].Content)
	}
	if !strings.Contains(got[2].Content, "<system-reminder>") {
		t.Errorf("last message should be attachment, got %q", got[2].Content)
	}
}

func TestAppendPostCompactAttachments_NoAttachments(t *testing.T) {
	compressed := []*schema.Message{{Role: schema.User, Content: "summary"}}
	discarded := []*schema.Message{{Role: schema.User, Content: "no reads here"}}
	h := &helpers{}
	got := appendPostCompactAttachments(context.Background(), h, compressed, discarded)
	if len(got) != 1 {
		t.Errorf("expected original compressed unchanged, got len %d", len(got))
	}
}
