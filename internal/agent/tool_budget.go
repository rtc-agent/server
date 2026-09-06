package agent

import (
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

const (
	// DefaultToolResultMaxTokens is the default maximum tokens for a single tool result.
	// At ~4 bytes/token, this equals ~40,000 characters.
	//
	// The value (10000) is chosen to prevent oversized tool outputs (e.g., large file
	// reads, long script outputs) from consuming excessive context space, while still
	// preserving enough content for the LLM to understand the tool's result.
	DefaultToolResultMaxTokens = 10000

	// ToolResultTruncateMsg is inserted between the preserved head and tail
	// when a tool result is truncated.
	ToolResultTruncateMsg = "\n\n[Content truncated - exceeded token limit]\n\n"
)

// ToolResultBudgetConfig controls the tool result size limiting behavior.
type ToolResultBudgetConfig struct {
	// MaxTokens is the maximum number of tokens allowed for a single tool result.
	// If <= 0, defaults to DefaultToolResultMaxTokens (10000).
	MaxTokens int
}

// DefaultToolResultBudgetConfig returns the default ToolResultBudget configuration.
func DefaultToolResultBudgetConfig() ToolResultBudgetConfig {
	return ToolResultBudgetConfig{
		MaxTokens: DefaultToolResultMaxTokens,
	}
}

// applyToolResultBudget truncates oversized tool results in-memory.
//
// Algorithm:
//  1. For each tool message, check if its content exceeds the token budget.
//  2. If yes, keep the first 60% and last 20% of the content, separated by
//     a truncation marker. The remaining 20% (middle) is discarded.
//  3. Return a new slice (original messages are not modified).
//
// This function operates in-memory only; it does NOT persist changes to the DB.
// The truncation is applied before the messages are sent to the LLM, so the
// original full content remains in the database for potential future retrieval.
//
// Token estimation uses ~4 bytes per token, consistent with the rest of the
// codebase (see summarize_middleware.go's estimateMessageTokens).
func applyToolResultBudget(messages []*turnagent.Message, cfg ToolResultBudgetConfig) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultToolResultMaxTokens
	}
	maxChars := maxTokens * 4 // rough estimate: 4 bytes per token

	result := make([]*turnagent.Message, len(messages))
	for i, msg := range messages {
		if msg.Role == turnagent.RoleTool && len(msg.Content) > maxChars {
			// Keep head 60% + tail 20%
			headChars := int(float64(maxChars) * 0.6)
			tailChars := int(float64(maxChars) * 0.2)

			head := msg.Content[:headChars]
			tail := msg.Content[len(msg.Content)-tailChars:]
			newContent := head + ToolResultTruncateMsg + tail

			result[i] = &turnagent.Message{
				Role:       msg.Role,
				Content:    newContent,
				ToolName:   msg.ToolName,
				ToolCallID: msg.ToolCallID,
				CreatedAt:  msg.CreatedAt,
			}
		} else {
			result[i] = msg
		}
	}

	return result
}
