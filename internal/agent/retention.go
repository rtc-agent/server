package agent

import (
	"github.com/cloudwego/eino/schema"
)

// RetentionConfig controls how many messages to keep after compression.
//
// The retention algorithm walks backward from the end of the message list,
// accumulating tokens and counting text-block messages (user/assistant with
// non-empty content). It stops when either:
//   - Both MinTokens and MinTextBlockMessages thresholds are met, OR
//   - MaxTokens is reached (hard upper limit).
//
// This ensures the compressed conversation retains enough recent context
// for the LLM to continue working effectively.
type RetentionConfig struct {
	// MinTokens is the minimum number of tokens to retain. The retention
	// boundary will not be set until at least this many tokens are accumulated.
	// Default: 10000.
	MinTokens int

	// MinTextBlockMessages is the minimum number of text-block messages
	// (user or assistant messages with non-empty Content) to retain.
	// Default: 5.
	MinTextBlockMessages int

	// MaxTokens is the maximum number of tokens to retain. The retention
	// boundary is forced at this point even if MinTokens/MinTextBlockMessages
	// are not yet met. Default: 40000.
	MaxTokens int
}

// DefaultRetentionConfig returns the default retention configuration.
func DefaultRetentionConfig() RetentionConfig {
	return RetentionConfig{
		MinTokens:            10000,
		MinTextBlockMessages: 5,
		MaxTokens:            40000,
	}
}

// calculateRetentionIndex determines the split point for compression.
//
// Messages at or after the returned index should be retained (not compressed).
// Messages before the index should be summarized.
//
// Returns 0 if all messages should be compressed (no retention).
// Returns len(msgs) if no messages should be compressed (skip compression).
//
// Algorithm:
//  1. Walk backward from the end, accumulating tokens and counting text blocks.
//  2. When both MinTokens AND MinTextBlockMessages are met → found boundary.
//  3. When MaxTokens is reached → force boundary.
//  4. Adjust boundary to preserve API invariants (tool_use/tool_result pairing).
func calculateRetentionIndex(msgs []*schema.Message, config RetentionConfig) int {
	if len(msgs) == 0 {
		return 0
	}

	totalTokens := 0
	textBlockCount := 0

	// Walk backward to find the retention boundary.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		totalTokens += estimateMessageTokensPrecise(msg)

		// Count text-block messages (user/assistant with non-empty content).
		if (msg.Role == schema.User || msg.Role == schema.Assistant) && msg.Content != "" {
			textBlockCount++
		}

		// Check if we've met the minimum retention criteria.
		if totalTokens >= config.MinTokens && textBlockCount >= config.MinTextBlockMessages {
			return adjustIndexToPreserveAPIInvariants(msgs, i)
		}

		// Force stop at max tokens.
		if totalTokens >= config.MaxTokens {
			return adjustIndexToPreserveAPIInvariants(msgs, i)
		}
	}

	// All messages fit within retention budget — compress nothing.
	return len(msgs)
}

// adjustIndexToPreserveAPIInvariants adjusts the retention boundary to ensure
// that tool_use/tool_result pairs are not split across the compression boundary.
//
// If a retained message contains a tool_result, the corresponding tool_use
// (in an assistant message) must also be retained. The function walks backward
// from the boundary to find and include the assistant messages with tool_calls.
//
// The function may move the boundary earlier (include more messages) to
// preserve these invariants.
func adjustIndexToPreserveAPIInvariants(msgs []*schema.Message, startIndex int) int {
	if startIndex <= 0 || startIndex >= len(msgs) {
		return startIndex
	}

	// Collect all tool_result IDs in the retained portion.
	toolResultIDs := make(map[string]bool)
	for i := startIndex; i < len(msgs); i++ {
		msg := msgs[i]
		if msg.Role == schema.Tool && msg.ToolCallID != "" {
			toolResultIDs[msg.ToolCallID] = true
		}
	}

	// If no tool_results in retained portion, no adjustment needed.
	if len(toolResultIDs) == 0 {
		return startIndex
	}

	// Check if any tool_use for the retained tool_results is already in the
	// retained portion. If so, remove it from the set (no need to pull it in).
	for i := startIndex; i < len(msgs); i++ {
		msg := msgs[i]
		if msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if toolResultIDs[tc.ID] {
					delete(toolResultIDs, tc.ID)
				}
			}
		}
	}

	// If all tool_uses are already retained, no adjustment needed.
	if len(toolResultIDs) == 0 {
		return startIndex
	}

	// Walk backward from startIndex to find the missing tool_uses.
	adjustedIndex := startIndex
	for len(toolResultIDs) > 0 && adjustedIndex > 0 {
		adjustedIndex--
		msg := msgs[adjustedIndex]

		if msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if toolResultIDs[tc.ID] {
					delete(toolResultIDs, tc.ID)
				}
			}
		}
	}

	return adjustedIndex
}

// shouldCompressByTokenCount returns true if the message list has enough
// content to warrant compression. This prevents compressing very short
// conversations where the overhead is not worthwhile.
//
// A minimum of 3 messages is required to avoid compressing trivial exchanges.
func shouldCompressByTokenCount(msgs []*schema.Message) bool {
	return len(msgs) >= 3
}
