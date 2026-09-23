package turnagent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Basic Merge Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_EmptyList(t *testing.T) {
	result := mergeAdjacentAssistantMessages([]*schema.Message{})
	assert.Empty(t, result)
}

func TestMergeAdjacentAssistantMessages_SingleMessage(t *testing.T) {
	msg := &schema.Message{Role: schema.Assistant, Content: "hello"}
	result := mergeAdjacentAssistantMessages([]*schema.Message{msg})
	require.Len(t, result, 1)
	assert.Equal(t, "hello", result[0].Content)
}

func TestMergeAdjacentAssistantMessages_NoAdjacentAssistants(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.User, Content: "user1"},
		{Role: schema.Assistant, Content: "assistant1"},
		{Role: schema.User, Content: "user2"},
		{Role: schema.Assistant, Content: "assistant2"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 4)
	// No merging should occur
	assert.Equal(t, "user1", result[0].Content)
	assert.Equal(t, "assistant1", result[1].Content)
	assert.Equal(t, "user2", result[2].Content)
	assert.Equal(t, "assistant2", result[3].Content)
}

// =============================================================================
// Core Merge Logic Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_TwoAdjacentAssistants(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1"},
		{Role: schema.Assistant, Content: "text2"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Content should be moved to AssistantGenMultiContent
	assert.Empty(t, result[0].Content)
	require.Len(t, result[0].AssistantGenMultiContent, 2)
	assert.Equal(t, "text1", result[0].AssistantGenMultiContent[0].Text)
	assert.Equal(t, "text2", result[0].AssistantGenMultiContent[1].Text)
}

func TestMergeAdjacentAssistantMessages_ThreeAdjacentAssistants(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1"},
		{Role: schema.Assistant, Content: "text2"},
		{Role: schema.Assistant, Content: "text3"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Chain merge: all three should be merged
	assert.Empty(t, result[0].Content)
	require.Len(t, result[0].AssistantGenMultiContent, 3)
	assert.Equal(t, "text1", result[0].AssistantGenMultiContent[0].Text)
	assert.Equal(t, "text2", result[0].AssistantGenMultiContent[1].Text)
	assert.Equal(t, "text3", result[0].AssistantGenMultiContent[2].Text)
}

func TestMergeAdjacentAssistantMessages_ContentAndToolCalls(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "let me read the file"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Function: schema.FunctionCall{Name: "read"}}}},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// 修复后：当 prev 有 Content 且 curr 只有 ToolCalls 时，保持 Content 字段不变
	// 这对 LLM 缓存命中至关重要：DB 加载合并后与内存直接累积的表示必须一致
	// 内存中 LLM 返回的消息是 Content+ToolCalls 形式，而不是 MultiContent+ToolCalls
	assert.Equal(t, "let me read the file", result[0].Content)
	assert.Nil(t, result[0].AssistantGenMultiContent)

	// ToolCalls should be merged
	require.Len(t, result[0].ToolCalls, 1)
	assert.Equal(t, "tc1", result[0].ToolCalls[0].ID)
}

func TestMergeAdjacentAssistantMessages_ToolCallsAndContent(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Assistant, Content: "now let me analyze"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// First message has no Content, so MultiContent should only have the second message's content
	require.Len(t, result[0].AssistantGenMultiContent, 1)
	assert.Equal(t, "now let me analyze", result[0].AssistantGenMultiContent[0].Text)

	// ToolCalls should be preserved
	require.Len(t, result[0].ToolCalls, 1)
	assert.Equal(t, "tc1", result[0].ToolCalls[0].ID)
}

func TestMergeAdjacentAssistantMessages_ComplexChain(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Assistant, Content: "text2", ToolCalls: []schema.ToolCall{{ID: "tc2", Function: schema.FunctionCall{Name: "write"}}}},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// MultiContent should have 2 text blocks
	require.Len(t, result[0].AssistantGenMultiContent, 2)
	assert.Equal(t, "text1", result[0].AssistantGenMultiContent[0].Text)
	assert.Equal(t, "text2", result[0].AssistantGenMultiContent[1].Text)

	// ToolCalls should be merged (2 total)
	require.Len(t, result[0].ToolCalls, 2)
	assert.Equal(t, "tc1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "tc2", result[0].ToolCalls[1].ID)
}

// =============================================================================
// ReasoningContent Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_ReasoningContent(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1", ReasoningContent: "thinking1"},
		{Role: schema.Assistant, Content: "text2", ReasoningContent: "thinking2"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// ReasoningContent should be concatenated with "\n"
	assert.Equal(t, "thinking1\nthinking2", result[0].ReasoningContent)
}

func TestMergeAdjacentAssistantMessages_ReasoningContentEmpty(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1", ReasoningContent: ""},
		{Role: schema.Assistant, Content: "text2", ReasoningContent: "thinking2"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Should not add extra "\n"
	assert.Equal(t, "thinking2", result[0].ReasoningContent)
}

func TestMergeAdjacentAssistantMessages_ReasoningContentPrevEmpty(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1", ReasoningContent: "thinking1"},
		{Role: schema.Assistant, Content: "text2", ReasoningContent: ""},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	assert.Equal(t, "thinking1", result[0].ReasoningContent)
}

// =============================================================================
// Extra Field Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_ExtraShallowMerge(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1", Extra: map[string]any{"key1": "value1"}},
		{Role: schema.Assistant, Content: "text2", Extra: map[string]any{"key2": "value2"}},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Extra should be merged
	assert.Equal(t, "value1", result[0].Extra["key1"])
	assert.Equal(t, "value2", result[0].Extra["key2"])
}

func TestMergeAdjacentAssistantMessages_ExtraConflict(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1", Extra: map[string]any{"key": "value1"}},
		{Role: schema.Assistant, Content: "text2", Extra: map[string]any{"key": "value2"}},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Latter should overwrite former (standard shallow merge)
	assert.Equal(t, "value2", result[0].Extra["key"])
}

// =============================================================================
// Claude Extra Key Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_ClaudeThinking(t *testing.T) {
	messages := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "text1",
			Extra:   map[string]any{"_eino_claude_thinking": "thinking1"},
		},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_thinking": "thinking2"},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Thinking should be concatenated with "\n"
	assert.Equal(t, "thinking1\nthinking2", result[0].Extra["_eino_claude_thinking"])
}

func TestMergeAdjacentAssistantMessages_ClaudeThinkingSignature(t *testing.T) {
	messages := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "text1",
			Extra:   map[string]any{"_eino_claude_thinking_signature": "sig1"},
		},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_thinking_signature": "sig2"},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Signature should keep the last one
	assert.Equal(t, "sig2", result[0].Extra["_eino_claude_thinking_signature"])
}

func TestMergeAdjacentAssistantMessages_ClaudeBreakpoint(t *testing.T) {
	messages := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "text1",
			Extra:   map[string]any{"_eino_claude_breakpoint": true},
		},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_breakpoint": false},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Prev's breakpoint should be preserved
	assert.Equal(t, true, result[0].Extra["_eino_claude_breakpoint"])
}

func TestMergeAdjacentAssistantMessages_ClaudeBreakpointInherit(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "text1"},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_breakpoint": true},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Should inherit from curr when prev doesn't have it
	assert.Equal(t, true, result[0].Extra["_eino_claude_breakpoint"])
}

func TestMergeAdjacentAssistantMessages_ClaudeBreakpointTTL(t *testing.T) {
	messages := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "text1",
			Extra:   map[string]any{"_eino_claude_breakpoint_ttl": "5m"},
		},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_breakpoint_ttl": "1h"},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Prev's TTL should be preserved
	assert.Equal(t, "5m", result[0].Extra["_eino_claude_breakpoint_ttl"])
}

func TestMergeAdjacentAssistantMessages_ClaudeToolSearchEvents(t *testing.T) {
	events1 := []claude.ToolSearchEvent{{Type: "server_tool_use", ID: "id1"}}
	events2 := []claude.ToolSearchEvent{{Type: "tool_search_tool_result", ToolUseID: "id1"}}

	messages := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "text1",
			Extra:   map[string]any{"_eino_claude_tool_search_events": events1},
		},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_tool_search_events": events2},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Events should be concatenated
	mergedEvents := result[0].Extra["_eino_claude_tool_search_events"].([]claude.ToolSearchEvent)
	require.Len(t, mergedEvents, 2)
	assert.Equal(t, "server_tool_use", mergedEvents[0].Type)
	assert.Equal(t, "tool_search_tool_result", mergedEvents[1].Type)
}

func TestMergeAdjacentAssistantMessages_ClaudeCacheCreationTokens(t *testing.T) {
	messages := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "text1",
			Extra:   map[string]any{"_eino_claude_cache_creation_input_tokens": 100},
		},
		{
			Role:    schema.Assistant,
			Content: "text2",
			Extra:   map[string]any{"_eino_claude_cache_creation_input_tokens": 200},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Tokens should be accumulated
	assert.Equal(t, 300, result[0].Extra["_eino_claude_cache_creation_input_tokens"])
}

// =============================================================================
// MultiContent Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_ExistingMultiContent(t *testing.T) {
	messages := []*schema.Message{
		{
			Role: schema.Assistant,
			AssistantGenMultiContent: []schema.MessageOutputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "text1"},
			},
		},
		{Role: schema.Assistant, Content: "text2"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Should append to existing MultiContent
	require.Len(t, result[0].AssistantGenMultiContent, 2)
	assert.Equal(t, "text1", result[0].AssistantGenMultiContent[0].Text)
	assert.Equal(t, "text2", result[0].AssistantGenMultiContent[1].Text)
}

func TestMergeAdjacentAssistantMessages_BothHaveMultiContent(t *testing.T) {
	messages := []*schema.Message{
		{
			Role: schema.Assistant,
			AssistantGenMultiContent: []schema.MessageOutputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "text1"},
			},
		},
		{
			Role: schema.Assistant,
			AssistantGenMultiContent: []schema.MessageOutputPart{
				{Type: schema.ChatMessagePartTypeText, Text: "text2"},
			},
		},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Should concatenate MultiContent slices
	require.Len(t, result[0].AssistantGenMultiContent, 2)
	assert.Equal(t, "text1", result[0].AssistantGenMultiContent[0].Text)
	assert.Equal(t, "text2", result[0].AssistantGenMultiContent[1].Text)
}

// =============================================================================
// Copy Isolation Tests
// =============================================================================

func TestMergeAdjacentAssistantMessages_NoModifyOriginal(t *testing.T) {
	original1 := &schema.Message{
		Role:      schema.Assistant,
		Content:   "text1",
		ToolCalls: []schema.ToolCall{{ID: "tc1", Function: schema.FunctionCall{Name: "read"}}},
	}
	original2 := &schema.Message{
		Role:    schema.Assistant,
		Content: "text2",
	}

	messages := []*schema.Message{original1, original2}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Original messages should not be modified
	assert.Equal(t, "text1", original1.Content)
	assert.Len(t, original1.ToolCalls, 1)
	assert.Equal(t, "text2", original2.Content)

	// Result should be a new merged message
	assert.Empty(t, result[0].Content)
	assert.Len(t, result[0].AssistantGenMultiContent, 2)
	assert.Len(t, result[0].ToolCalls, 1)
}

// =============================================================================
// Middleware Integration Tests
// =============================================================================

func TestMergeAssistantMiddleware_BeforeModelRewriteState(t *testing.T) {
	mw := NewMergeAssistantMiddleware(&MergeAssistantMiddlewareConfig{})

	state := &adk.ChatModelAgentState{
		Messages: []*schema.Message{
			{Role: schema.Assistant, Content: "text1"},
			{Role: schema.Assistant, Content: "text2"},
		},
	}

	ctx := context.Background()
	mc := &adk.ModelContext{}

	newCtx, newState, err := mw.BeforeModelRewriteState(ctx, state, mc)
	require.NoError(t, err)
	assert.Equal(t, ctx, newCtx)
	require.Len(t, newState.Messages, 1)

	// Messages should be merged
	assert.Empty(t, newState.Messages[0].Content)
	require.Len(t, newState.Messages[0].AssistantGenMultiContent, 2)
}

func TestMergeAssistantMiddleware_EmptyMessages(t *testing.T) {
	mw := NewMergeAssistantMiddleware(&MergeAssistantMiddlewareConfig{})

	state := &adk.ChatModelAgentState{Messages: []*schema.Message{}}
	ctx := context.Background()
	mc := &adk.ModelContext{}

	newCtx, newState, err := mw.BeforeModelRewriteState(ctx, state, mc)
	require.NoError(t, err)
	assert.Equal(t, ctx, newCtx)
	assert.Empty(t, newState.Messages)
}

func TestMergeAssistantMiddleware_WithMetrics(t *testing.T) {
	var recordedDuration float64
	var recordedCount int

	mw := NewMergeAssistantMiddleware(&MergeAssistantMiddlewareConfig{
		RecordDuration:    func(ms float64) { recordedDuration = ms },
		RecordMergedCount: func(count int) { recordedCount = count },
	})

	state := &adk.ChatModelAgentState{
		Messages: []*schema.Message{
			{Role: schema.Assistant, Content: "text1"},
			{Role: schema.Assistant, Content: "text2"},
			{Role: schema.Assistant, Content: "text3"},
		},
	}

	ctx := context.Background()
	mc := &adk.ModelContext{}

	_, newState, err := mw.BeforeModelRewriteState(ctx, state, mc)
	require.NoError(t, err)
	require.Len(t, newState.Messages, 1)

	// Metrics should be recorded
	assert.GreaterOrEqual(t, recordedDuration, float64(0))
	assert.Equal(t, 2, recordedCount) // 3 -> 1, so 2 messages merged
}

// =============================================================================
// Edge Cases
// =============================================================================

func TestMergeAdjacentAssistantMessages_EmptyContent(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: ""},
		{Role: schema.Assistant, Content: "text2"},
	}
	result := mergeAdjacentAssistantMessages(messages)
	require.Len(t, result, 1)

	// Empty content should not create empty text block
	require.Len(t, result[0].AssistantGenMultiContent, 1)
	assert.Equal(t, "text2", result[0].AssistantGenMultiContent[0].Text)
}

func TestMergeAdjacentAssistantMessages_UserToolAssistant(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.User, Content: "user"},
		{Role: schema.Tool, Content: "tool result", ToolCallID: "tc1"},
		{Role: schema.Assistant, Content: "assistant"},
	}
	result := mergeAdjacentAssistantMessages(messages)

	// No merging should occur (different roles)
	require.Len(t, result, 3)
}

// =============================================================================
// normalizeToolCallOrdering Tests
// =============================================================================

func TestNormalizeToolCallOrdering_EmptyMessages(t *testing.T) {
	t.Parallel()
	result := normalizeToolCallOrdering(nil)
	assert.Nil(t, result)

	result = normalizeToolCallOrdering([]*schema.Message{})
	assert.Empty(t, result)
}

func TestNormalizeToolCallOrdering_SingleToolCall(t *testing.T) {
	t.Parallel()
	// Single tool_call — no reordering should occur
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc1", Function: schema.FunctionCall{Name: "read", Arguments: `{"path":"/a"}`}},
		}},
		{Role: schema.Tool, Content: "result1", ToolCallID: "tc1"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 2)
	assert.Equal(t, "tc1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "tc1", result[1].ToolCallID)
}

func TestNormalizeToolCallOrdering_AlreadySorted(t *testing.T) {
	t.Parallel()
	// tool_calls already in alphabetical order — no mutation
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc1", Function: schema.FunctionCall{Name: "listLoops", Arguments: "{}"}},
			{ID: "tc2", Function: schema.FunctionCall{Name: "read", Arguments: `{"path":"/a"}`}},
		}},
		{Role: schema.Tool, Content: "result_loops", ToolCallID: "tc1"},
		{Role: schema.Tool, Content: "result_read", ToolCallID: "tc2"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 3)

	// tool_calls order preserved
	assert.Equal(t, "tc1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "tc2", result[0].ToolCalls[1].ID)
	// tool_results order preserved
	assert.Equal(t, "tc1", result[1].ToolCallID)
	assert.Equal(t, "tc2", result[2].ToolCallID)
}

func TestNormalizeToolCallOrdering_SortsByName(t *testing.T) {
	t.Parallel()
	// tool_calls: [read, listLoops] → sorted: [listLoops, read]
	// tool_results: [read_result, listLoops_result] → reordered: [listLoops_result, read_result]
	messages := []*schema.Message{
		{Role: schema.Assistant, Content: "Let me check.", ToolCalls: []schema.ToolCall{
			{ID: "tc_read", Function: schema.FunctionCall{Name: "read", Arguments: `{"path":"/a"}`}},
			{ID: "tc_loops", Function: schema.FunctionCall{Name: "listLoops", Arguments: "{}"}},
		}},
		{Role: schema.Tool, Content: "read_result", ToolCallID: "tc_read"},
		{Role: schema.Tool, Content: "loops_result", ToolCallID: "tc_loops"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 3)

	// assistant tool_calls sorted: listLoops < read
	assert.Equal(t, "tc_loops", result[0].ToolCalls[0].ID)
	assert.Equal(t, "tc_read", result[0].ToolCalls[1].ID)
	// tool_results reordered to match
	assert.Equal(t, "tc_loops", result[1].ToolCallID)
	assert.Equal(t, "loops_result", result[1].Content)
	assert.Equal(t, "tc_read", result[2].ToolCallID)
	assert.Equal(t, "read_result", result[2].Content)
}

func TestNormalizeToolCallOrdering_SameToolDifferentArgs(t *testing.T) {
	t.Parallel()
	// Same tool called twice with different args — sorted by arguments
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc_b", Function: schema.FunctionCall{Name: "read", Arguments: `{"path":"/b"}`}},
			{ID: "tc_a", Function: schema.FunctionCall{Name: "read", Arguments: `{"path":"/a"}`}},
		}},
		{Role: schema.Tool, Content: "result_b", ToolCallID: "tc_b"},
		{Role: schema.Tool, Content: "result_a", ToolCallID: "tc_a"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 3)

	// Sorted by arguments: {"path":"/a"} < {"path":"/b"}
	assert.Equal(t, "tc_a", result[0].ToolCalls[0].ID)
	assert.Equal(t, "tc_b", result[0].ToolCalls[1].ID)
	assert.Equal(t, "tc_a", result[1].ToolCallID)
	assert.Equal(t, "result_a", result[1].Content)
	assert.Equal(t, "tc_b", result[2].ToolCallID)
	assert.Equal(t, "result_b", result[2].Content)
}

func TestNormalizeToolCallOrdering_MismatchedCount(t *testing.T) {
	t.Parallel()
	// 2 tool_calls but 3 tool_results — count mismatch, skip reordering
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc2", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			{ID: "tc1", Function: schema.FunctionCall{Name: "listLoops", Arguments: "{}"}},
		}},
		{Role: schema.Tool, Content: "r1", ToolCallID: "tc1"},
		{Role: schema.Tool, Content: "r2", ToolCallID: "tc2"},
		{Role: schema.Tool, Content: "r_orphan", ToolCallID: "tc_orphan"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 4)

	// tool_calls should still be sorted (the assistant msg is always updated)
	assert.Equal(t, "tc1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "tc2", result[0].ToolCalls[1].ID)
	// tool_results unchanged (count mismatch → skip reordering)
	assert.Equal(t, "tc1", result[1].ToolCallID)
	assert.Equal(t, "tc2", result[2].ToolCallID)
	assert.Equal(t, "tc_orphan", result[3].ToolCallID)
}

func TestNormalizeToolCallOrdering_NoToolResults(t *testing.T) {
	t.Parallel()
	// assistant with tool_calls but no following tool results
	messages := []*schema.Message{
		{Role: schema.User, Content: "go"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc2", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			{ID: "tc1", Function: schema.FunctionCall{Name: "listLoops", Arguments: "{}"}},
		}},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 2)
	// tool_calls sorted
	assert.Equal(t, "tc1", result[1].ToolCalls[0].ID)
	assert.Equal(t, "tc2", result[1].ToolCalls[1].ID)
}

func TestNormalizeToolCallOrdering_MultipleGroups(t *testing.T) {
	t.Parallel()
	// Two assistant messages with tool_calls, each followed by tool_results
	messages := []*schema.Message{
		{Role: schema.User, Content: "start"},
		// Group 1
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc_b", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			{ID: "tc_a", Function: schema.FunctionCall{Name: "grep", Arguments: "{}"}},
		}},
		{Role: schema.Tool, Content: "r_b", ToolCallID: "tc_b"},
		{Role: schema.Tool, Content: "r_a", ToolCallID: "tc_a"},
		// Group 2
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc_d", Function: schema.FunctionCall{Name: "write", Arguments: "{}"}},
			{ID: "tc_c", Function: schema.FunctionCall{Name: "find", Arguments: "{}"}},
		}},
		{Role: schema.Tool, Content: "r_d", ToolCallID: "tc_d"},
		{Role: schema.Tool, Content: "r_c", ToolCallID: "tc_c"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 7)

	// Group 1: grep < read
	assert.Equal(t, "tc_a", result[1].ToolCalls[0].ID) // grep
	assert.Equal(t, "tc_b", result[1].ToolCalls[1].ID) // read
	assert.Equal(t, "tc_a", result[2].ToolCallID)
	assert.Equal(t, "tc_b", result[3].ToolCallID)

	// Group 2: find < write
	assert.Equal(t, "tc_c", result[4].ToolCalls[0].ID) // find
	assert.Equal(t, "tc_d", result[4].ToolCalls[1].ID) // write
	assert.Equal(t, "tc_c", result[5].ToolCallID)
	assert.Equal(t, "tc_d", result[6].ToolCallID)
}

func TestNormalizeToolCallOrdering_DoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	// Verify original messages are not mutated
	original := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "tc_b", Function: schema.FunctionCall{Name: "read", Arguments: "{}"}},
			{ID: "tc_a", Function: schema.FunctionCall{Name: "grep", Arguments: "{}"}},
		}},
		{Role: schema.Tool, Content: "r_b", ToolCallID: "tc_b"},
		{Role: schema.Tool, Content: "r_a", ToolCallID: "tc_a"},
	}
	_ = normalizeToolCallOrdering(original)

	// Original assistant message's ToolCalls order unchanged
	assert.Equal(t, "tc_b", original[0].ToolCalls[0].ID)
	assert.Equal(t, "tc_a", original[0].ToolCalls[1].ID)
	// Original tool results unchanged
	assert.Equal(t, "tc_b", original[1].ToolCallID)
	assert.Equal(t, "tc_a", original[2].ToolCallID)
}

func TestNormalizeToolCallOrdering_NonAssistantMessages(t *testing.T) {
	t.Parallel()
	// No assistant messages — should pass through unchanged
	messages := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Tool, Content: "result", ToolCallID: "tc1"},
	}
	result := normalizeToolCallOrdering(messages)
	require.Len(t, result, 2)
	assert.Equal(t, "hello", result[0].Content)
	assert.Equal(t, "result", result[1].Content)
}

// =============================================================================
// deduplicateToolResults Tests
// =============================================================================

func TestDeduplicateToolResults_EmptyAndSingle(t *testing.T) {
	t.Parallel()
	assert.Empty(t, deduplicateToolResults(nil))
	assert.Empty(t, deduplicateToolResults([]*schema.Message{}))

	// Single tool message — no dedup needed
	single := []*schema.Message{{Role: schema.Tool, ToolCallID: "tc1", Content: "r1"}}
	result := deduplicateToolResults(single)
	require.Len(t, result, 1)
	assert.Equal(t, "tc1", result[0].ToolCallID)
}

func TestDeduplicateToolResults_NoDuplicates(t *testing.T) {
	t.Parallel()
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc1"}, {ID: "tc2"}}},
		{Role: schema.Tool, ToolCallID: "tc1", Content: "r1"},
		{Role: schema.Tool, ToolCallID: "tc2", Content: "r2"},
	}
	result := deduplicateToolResults(messages)
	require.Len(t, result, 3)
	assert.Equal(t, "r1", result[1].Content)
	assert.Equal(t, "r2", result[2].Content)
}

func TestDeduplicateToolResults_RemovesDuplicate(t *testing.T) {
	t.Parallel()
	// BUG 17 scenario: 1 tool_use, 2 identical tool_results
	messages := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc1"}}},
		{Role: schema.Tool, ToolCallID: "tc1", Content: "first"},
		{Role: schema.Tool, ToolCallID: "tc1", Content: "duplicate"},
	}
	result := deduplicateToolResults(messages)
	require.Len(t, result, 2, "duplicate tool result should be removed")
	assert.Equal(t, "tc1", result[1].ToolCallID)
	assert.Equal(t, "first", result[1].Content, "first occurrence kept")
}

func TestDeduplicateToolResults_PreservesNonToolMessages(t *testing.T) {
	t.Parallel()
	messages := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "hi"},
		{Role: schema.Tool, ToolCallID: "tc1", Content: "r1"},
		{Role: schema.Tool, ToolCallID: "tc1", Content: "r1-dup"},
		{Role: schema.User, Content: "next"},
	}
	result := deduplicateToolResults(messages)
	require.Len(t, result, 4)
	assert.Equal(t, schema.User, result[0].Role)
	assert.Equal(t, schema.Assistant, result[1].Role)
	assert.Equal(t, "r1", result[2].Content)
	assert.Equal(t, "next", result[3].Content)
}

func TestDeduplicateToolResults_ToolWithEmptyID(t *testing.T) {
	t.Parallel()
	// Tool messages with empty ToolCallID should not be deduplicated
	messages := []*schema.Message{
		{Role: schema.Tool, ToolCallID: "", Content: "r1"},
		{Role: schema.Tool, ToolCallID: "", Content: "r2"},
	}
	result := deduplicateToolResults(messages)
	require.Len(t, result, 2, "empty ToolCallID should not trigger dedup")
}
