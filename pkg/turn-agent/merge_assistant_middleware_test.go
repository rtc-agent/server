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
