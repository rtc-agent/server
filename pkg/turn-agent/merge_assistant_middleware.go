// Package turnagent — merge_assistant_middleware.go
//
// # Merge Assistant Middleware
//
// This middleware merges adjacent assistant messages in state.Messages before
// each ChatModel invocation. It solves the cache invalidation bug caused by
// unmerged assistant messages in the ReAct loop.
//
// Problem:
//   - normalizeMessagesForLLM runs only once during GenInput phase
//   - ReAct loop iterations accumulate messages in state.Messages
//   - Adjacent assistant messages (from different LLM responses) are not merged
//   - This causes cache invalidation and API spec violations
//
// Solution:
//   - MergeAssistantMiddleware runs before EVERY ChatModel call
//   - Merges adjacent assistant messages using AssistantGenMultiContent
//   - Avoids "\n" concatenation (which causes content pollution)
//   - Preserves Claude-specific Extra keys (thinking, breakpoint, etc.)
//
// Architecture:
//   - Operates on schema.Message (eino layer), not turnagent.Message
//   - Complements normalizeMessagesForLLM (which handles turnagent.Message)
//   - Uses AssistantGenMultiContent for multi-content-block merging
//   - Compatible with Claude adapter's convSchemaMessage (if-else-if chain)

package turnagent

import (
	"context"
	"sort"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// =============================================================================
// Middleware Configuration
// =============================================================================

// MergeAssistantMiddlewareConfig defines the configuration for the merge assistant middleware.
type MergeAssistantMiddlewareConfig struct {
	// Log is an optional logger. If nil, no logs are emitted.
	Log Logger

	// RecordDuration is an optional callback for recording merge operation duration.
	// Called with duration in milliseconds after each merge operation.
	RecordDuration func(durationMs float64)

	// RecordMergedCount is an optional callback for recording the number of merged messages.
	// Called with the count of messages that were merged (before - after).
	RecordMergedCount func(count int)
}

// =============================================================================
// Middleware Implementation
// =============================================================================

// mergeAssistantMiddleware implements adk.ChatModelAgentMiddleware for merging
// adjacent assistant messages.
type mergeAssistantMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	cfg *MergeAssistantMiddlewareConfig
}

// NewMergeAssistantMiddleware creates a middleware that merges adjacent assistant
// messages before each ChatModel invocation.
//
// The middleware hooks into the eino ChatModelAgent lifecycle:
//
//   - BeforeModelRewriteState: merges adjacent assistant messages in state.Messages
//     to ensure consistent message structure across ReAct iterations.
//
// Usage:
//
//	mw := turnagent.NewMergeAssistantMiddleware(&turnagent.MergeAssistantMiddlewareConfig{
//	    Log: logger,
//	    RecordDuration: func(ms float64) { metrics.RecordMergeDuration(ms) },
//	    RecordMergedCount: func(count int) { metrics.RecordMergeCount(count) },
//	})
//	cfg := turnagent.Config{
//	    AgentMiddlewares: []adk.ChatModelAgentMiddleware{mw, summarizeMW},
//	    // ...
//	}
//
// The middleware is opt-in. If Config.AgentMiddlewares is empty or does not
// include this middleware, no merging occurs.
func NewMergeAssistantMiddleware(cfg *MergeAssistantMiddlewareConfig) adk.ChatModelAgentMiddleware {
	if cfg == nil {
		cfg = &MergeAssistantMiddlewareConfig{}
	}

	if cfg.Log != nil {
		cfg.Log.Debug(context.Background(), "merge_assistant_middleware.created", map[string]any{
			"has_log":             cfg.Log != nil,
			"has_record_duration": cfg.RecordDuration != nil,
			"has_record_count":    cfg.RecordMergedCount != nil,
		})
	}

	return &mergeAssistantMiddleware{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		cfg:                          cfg,
	}
}

// log dispatches a log call to the appropriate Logger method based on the
// level string. Used internally to keep call sites compact.
//
// Delegates to shared middlewareLog utility in middleware_utils.go.
func (m *mergeAssistantMiddleware) log(ctx context.Context, level, msg string, attrs map[string]any) {
	middlewareLog(m.cfg.Log, ctx, level, msg, attrs)
}

// BeforeModelRewriteState is called before each model invocation. It merges
// adjacent assistant messages to ensure consistent message structure.
func (m *mergeAssistantMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	mc *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if len(state.Messages) == 0 {
		return ctx, state, nil
	}

	// DEBUG: Log message roles to trace system message presence
	roles := make([]string, 0, min(10, len(state.Messages)))
	for i, msg := range state.Messages {
		if i >= 10 {
			break
		}
		roles = append(roles, string(msg.Role))
	}
	m.log(ctx, "info", "merge_assistant_middleware.debug_roles", map[string]any{
		"total_messages": len(state.Messages),
		"first_10_roles": roles,
	})

	start := time.Now()
	beforeCount := len(state.Messages)

	// Merge adjacent assistant messages
	state.Messages = mergeAdjacentAssistantMessages(state.Messages)

	// Normalize tool call ordering for cache consistency.
	// Ensures deterministic (tool_name, arguments) ordering of tool_calls
	// and matching tool_results, preventing cache invalidation from
	// non-deterministic RTC submit timing or LLM tool_call ordering.
	state.Messages = normalizeToolCallOrdering(state.Messages)

	afterCount := len(state.Messages)
	durationMs := float64(time.Since(start).Milliseconds())

	// Record metrics
	if m.cfg.RecordDuration != nil {
		m.cfg.RecordDuration(durationMs)
	}
	if m.cfg.RecordMergedCount != nil && beforeCount > afterCount {
		m.cfg.RecordMergedCount(beforeCount - afterCount)
	}

	// Log the merge operation
	m.log(ctx, "debug", "merge_assistant_middleware.result", map[string]any{
		"before_count": beforeCount,
		"after_count":  afterCount,
		"merged_count": beforeCount - afterCount,
		"duration_ms":  durationMs,
	})

	return ctx, state, nil
}

// =============================================================================
// Core Merge Logic
// =============================================================================

// mergeAdjacentAssistantMessages merges adjacent assistant schema.Messages.
// Uses AssistantGenMultiContent for multi-content-block merging (not "\n" concatenation).
func mergeAdjacentAssistantMessages(messages []*schema.Message) []*schema.Message {
	if len(messages) <= 1 {
		return messages
	}

	var result []*schema.Message
	for _, msg := range messages {
		if len(result) > 0 &&
			result[len(result)-1].Role == schema.Assistant &&
			msg.Role == schema.Assistant {
			// Merge into previous assistant message
			prev := result[len(result)-1]
			mergeAssistantSchemaMessages(prev, msg)
		} else {
			// Copy message (avoid modifying original)
			result = append(result, copySchemaMessage(msg))
		}
	}

	return result
}

// =============================================================================
// Tool Call Ordering Normalization
// =============================================================================

// normalizeToolCallOrdering ensures deterministic ordering of tool calls
// and their corresponding tool results within assistant message groups.
//
// Problem:
//   - Parallel tool calls may produce tool_results in non-deterministic order
//     (RTC tools: client submits results in arbitrary order → global_offset varies)
//   - LLM may also vary tool_call ordering across requests
//   - Different ordering between requests → Anthropic prompt cache invalidation
//
// Solution:
//   - For each assistant message with multiple tool_calls, sort by (name, arguments)
//   - Reorder the immediately following tool result messages to match
//   - This ensures the message sequence is deterministic regardless of execution order
//
// This function runs in MergeAssistantMiddleware.BeforeModelRewriteState,
// which executes before EVERY ChatModel call — covering both the DB load path
// (GenInput) and the ReAct loop path (ToolNode execution → next ChatModel call).
func normalizeToolCallOrdering(messages []*schema.Message) []*schema.Message {
	if len(messages) == 0 {
		return messages
	}

	result := make([]*schema.Message, 0, len(messages))
	i := 0

	for i < len(messages) {
		msg := messages[i]
		result = append(result, msg)
		i++

		// Only process assistant messages with multiple tool calls
		if msg.Role != schema.Assistant || len(msg.ToolCalls) <= 1 {
			continue
		}

		// Copy and sort tool_calls by (name, arguments) for determinism
		sortedCalls := make([]schema.ToolCall, len(msg.ToolCalls))
		copy(sortedCalls, msg.ToolCalls)
		sort.SliceStable(sortedCalls, func(a, b int) bool {
			if sortedCalls[a].Function.Name != sortedCalls[b].Function.Name {
				return sortedCalls[a].Function.Name < sortedCalls[b].Function.Name
			}
			return sortedCalls[a].Function.Arguments < sortedCalls[b].Function.Arguments
		})

		// Check if order actually changed
		orderChanged := false
		for j := range sortedCalls {
			if sortedCalls[j].ID != msg.ToolCalls[j].ID {
				orderChanged = true
				break
			}
		}

		if !orderChanged {
			// Already sorted — pass through tool results as-is
			for i < len(messages) && messages[i].Role == schema.Tool {
				result = append(result, messages[i])
				i++
			}
			continue
		}

		// Update assistant message with sorted tool calls (shallow copy)
		sorted := *msg
		sorted.ToolCalls = sortedCalls
		result[len(result)-1] = &sorted

		// Build expected tool_call_id order from sorted calls
		expectedOrder := make([]string, len(sortedCalls))
		for j, tc := range sortedCalls {
			expectedOrder[j] = tc.ID
		}

		// Collect subsequent tool result messages
		toolResultStart := len(result)
		for i < len(messages) && messages[i].Role == schema.Tool {
			result = append(result, messages[i])
			i++
		}
		toolResultEnd := len(result)

		// Reorder tool results to match sorted tool calls.
		// Only reorder when count matches exactly; otherwise leave as-is
		// (repairToolPairing will handle mismatched counts).
		if toolResultEnd-toolResultStart == len(expectedOrder) {
			resultByID := make(map[string]*schema.Message, toolResultEnd-toolResultStart)
			for j := toolResultStart; j < toolResultEnd; j++ {
				if result[j].ToolCallID != "" {
					resultByID[result[j].ToolCallID] = result[j]
				}
			}
			for j, callID := range expectedOrder {
				if m, ok := resultByID[callID]; ok {
					result[toolResultStart+j] = m
				}
			}
		}
	}

	return result
}

// mergeAssistantSchemaMessages merges curr's content into prev.
// Uses AssistantGenMultiContent for text blocks (not "\n" concatenation).
//
// Key points:
//   - Content is moved to AssistantGenMultiContent (Claude adapter ignores Content when MultiContent is present)
//   - ToolCalls are appended directly
//   - ReasoningContent uses "\n" (thinking blocks are typically continuous)
//   - Extra keys are merged with special handling for Claude-specific keys
func mergeAssistantSchemaMessages(prev, curr *schema.Message) {
	// Merge Content/MultiContent
	mergeAssistantContent(prev, curr)

	// Merge ToolCalls (append directly)
	if len(curr.ToolCalls) > 0 {
		prev.ToolCalls = append(prev.ToolCalls, curr.ToolCalls...)
	}

	// Merge ReasoningContent (still use "\n", as thinking blocks are typically continuous)
	mergeAssistantReasoning(prev, curr)

	// Merge Extra (special handling for Claude-specific keys)
	mergeAssistantExtra(prev, curr)
}

// mergeAssistantContent merges curr's Content/MultiContent into prev using AssistantGenMultiContent.
func mergeAssistantContent(prev, curr *schema.Message) {
	// 特殊情况：prev 有 Content，curr 只有 ToolCalls（无 Content/MultiContent）
	// 保持 Content 字段不变，避免不必要的 MultiContent 转换
	// 这对 LLM 缓存命中至关重要：DB 加载合并后与内存直接累积的表示必须一致
	if prev.Content != "" && prev.AssistantGenMultiContent == nil &&
		curr.Content == "" && len(curr.AssistantGenMultiContent) == 0 && len(curr.ToolCalls) > 0 {
		// 无需修改 Content/MultiContent
		// Content 保持不变，ToolCalls 会在 mergeAssistantToolCalls 中合并
		return
	}

	// Determine if we need to initialize MultiContent
	// This happens when prev has Content and we're merging anything from curr
	needsMultiContentInit := prev.Content != "" && prev.AssistantGenMultiContent == nil &&
		(curr.Content != "" || len(curr.AssistantGenMultiContent) > 0 || len(curr.ToolCalls) > 0)

	if curr.Content != "" {
		// Initialize AssistantGenMultiContent if not already present
		if prev.AssistantGenMultiContent == nil {
			prev.AssistantGenMultiContent = []schema.MessageOutputPart{}
			if prev.Content != "" {
				prev.AssistantGenMultiContent = append(prev.AssistantGenMultiContent,
					schema.MessageOutputPart{
						Type: schema.ChatMessagePartTypeText,
						Text: prev.Content,
					})
				prev.Content = ""
			}
		}
		prev.AssistantGenMultiContent = append(prev.AssistantGenMultiContent,
			schema.MessageOutputPart{
				Type: schema.ChatMessagePartTypeText,
				Text: curr.Content,
			})
	} else if len(curr.AssistantGenMultiContent) > 0 {
		if prev.AssistantGenMultiContent == nil {
			prev.AssistantGenMultiContent = []schema.MessageOutputPart{}
			if prev.Content != "" {
				prev.AssistantGenMultiContent = append(prev.AssistantGenMultiContent,
					schema.MessageOutputPart{
						Type: schema.ChatMessagePartTypeText,
						Text: prev.Content,
					})
				prev.Content = ""
			}
		}
		prev.AssistantGenMultiContent = append(prev.AssistantGenMultiContent, curr.AssistantGenMultiContent...)
	} else if needsMultiContentInit {
		// curr has ToolCalls but no Content/MultiContent
		prev.AssistantGenMultiContent = []schema.MessageOutputPart{
			{
				Type: schema.ChatMessagePartTypeText,
				Text: prev.Content,
			},
		}
		prev.Content = ""
	}
}

// mergeAssistantReasoning merges curr's ReasoningContent into prev.
//
// ⚠️ CRITICAL: Claude adapter's convSchemaMessage (claude.go:1060) reads thinking
// from Extra["_eino_claude_thinking"] via GetThinking(), NOT from ReasoningContent.
// Therefore, we MUST sync update the Extra key in mergeAssistantExtra, otherwise
// thinking content will be lost in the API request.
func mergeAssistantReasoning(prev, curr *schema.Message) {
	if curr.ReasoningContent != "" {
		if prev.ReasoningContent != "" {
			prev.ReasoningContent = prev.ReasoningContent + "\n" + curr.ReasoningContent
		} else {
			prev.ReasoningContent = curr.ReasoningContent
		}
	}
}

// mergeAssistantExtra merges curr's Extra into prev with special handling for Claude-specific keys.
//
// Claude adapter uses Extra to store critical information (message_extra.go:44-51):
//   - "_eino_claude_thinking": thinking content (read by GetThinking)
//   - "_eino_claude_thinking_signature": thinking signature (for API validation)
//   - "_eino_claude_breakpoint": cache breakpoint marker
//   - "_eino_claude_breakpoint_ttl": cache breakpoint TTL ("1h" or "5m")
//   - "_eino_claude_tool_search_events": tool search events ([]ToolSearchEvent)
//   - "_eino_claude_cache_creation_input_tokens": cache creation input tokens (int)
//
// These keys cannot be simply overwritten; they require special handling.
func mergeAssistantExtra(prev, curr *schema.Message) {
	if curr.Extra == nil {
		return
	}
	if prev.Extra == nil {
		prev.Extra = make(map[string]any)
	}
	for k, v := range curr.Extra {
		mergeExtraKey(prev.Extra, k, v)
	}
}

// mergeExtraKey merges a single Extra key with special handling for Claude-specific keys.
func mergeExtraKey(prevExtra map[string]any, key string, value any) {
	switch key {
	case "_eino_claude_thinking":
		// Thinking content must be concatenated (consistent with ReasoningContent)
		if currThinking, ok := value.(string); ok && currThinking != "" {
			if prevThinking, ok := prevExtra[key].(string); ok && prevThinking != "" {
				prevExtra[key] = prevThinking + "\n" + currThinking
			} else {
				prevExtra[key] = currThinking
			}
		}
	case "_eino_claude_thinking_signature":
		// Signature: keep curr's (last thinking's signature)
		prevExtra[key] = value
	case "_eino_claude_breakpoint", "_eino_claude_breakpoint_ttl":
		// Breakpoint/TTL: if prev already has it, keep prev's; otherwise inherit curr's
		if _, hasPrev := prevExtra[key]; !hasPrev {
			prevExtra[key] = value
		}
	case "_eino_claude_tool_search_events":
		// If prev already has events, merge two slices; otherwise inherit curr's
		// Note: Extra stores []claude.ToolSearchEvent (strongly typed), not []any
		if prevEvents, ok := prevExtra[key].([]claude.ToolSearchEvent); ok && len(prevEvents) > 0 {
			if currEvents, ok := value.([]claude.ToolSearchEvent); ok {
				prevExtra[key] = append(prevEvents, currEvents...)
			}
		} else {
			prevExtra[key] = value
		}
	case "_eino_claude_cache_creation_input_tokens":
		// Cache creation tokens must be accumulated (not overwritten) to reflect
		// the total cache write cost after merging
		if prevTokens, ok := prevExtra[key].(int); ok && prevTokens > 0 {
			if currTokens, ok := value.(int); ok {
				prevExtra[key] = prevTokens + currTokens
			}
		} else {
			prevExtra[key] = value
		}
	default:
		// Other keys: direct overwrite (standard shallow merge)
		prevExtra[key] = value
	}
}

// copySchemaMessage creates a shallow copy of the message (avoids modifying original).
// Note: ToolCalls and AssistantGenMultiContent are deep-copied (slice copy).
// Extra field is shallow-copied (map key-value copy). If Extra values are pointers
// or slices, modifications will affect the original. This is acceptable for the
// current use case since Extra values are not modified after copying.
func copySchemaMessage(msg *schema.Message) *schema.Message {
	copied := *msg
	if msg.ToolCalls != nil {
		copied.ToolCalls = make([]schema.ToolCall, len(msg.ToolCalls))
		copy(copied.ToolCalls, msg.ToolCalls)
	}
	if msg.Extra != nil {
		copied.Extra = make(map[string]any)
		for k, v := range msg.Extra {
			copied.Extra[k] = v // Shallow copy
		}
	}
	if msg.AssistantGenMultiContent != nil {
		copied.AssistantGenMultiContent = make([]schema.MessageOutputPart, len(msg.AssistantGenMultiContent))
		copy(copied.AssistantGenMultiContent, msg.AssistantGenMultiContent)
	}
	return &copied
}
