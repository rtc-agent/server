package agent

import (
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// COMPACTABLE_TOOLS lists the tools whose results can be cleared by
// Microcompact. These tools typically produce large outputs that are not
// needed once the conversation has moved on.
//
// The tool names must match those registered in tools.go's createTools.
var COMPACTABLE_TOOLS = map[string]bool{
	"read":   true,
	"write":  true,
	"grep":   true,
	"find":   true,
	"script": true,
}

// TIME_BASED_MC_CLEARED_MESSAGE is the placeholder text that replaces the
// content of a cleared tool result.
const TIME_BASED_MC_CLEARED_MESSAGE = "[Old tool result content cleared]"

// MicrocompactConfig controls the behavior of time-based Microcompact.
type MicrocompactConfig struct {
	// GapThresholdMinutes is the minimum time gap (in minutes) since the last
	// assistant message before Microcompact triggers. When the user has been
	// idle for at least this long, old tool results are cleared.
	// Default: 60.
	GapThresholdMinutes int

	// KeepRecent is the number of most recent compactable tool results to
	// preserve. At least 1 is always kept (Math.max(1, KeepRecent)).
	// Default: 5.
	KeepRecent int
}

// DefaultMicrocompactConfig returns the default Microcompact configuration.
func DefaultMicrocompactConfig() MicrocompactConfig {
	return MicrocompactConfig{
		GapThresholdMinutes: 60,
		KeepRecent:          5,
	}
}

// microcompactMessages applies time-based Microcompact to the given messages.
//
// Algorithm:
//  1. Find the last assistant message and check if the time gap since it
//     exceeds GapThresholdMinutes. If not, skip (user is still active).
//  2. Collect all compactable tool call IDs in order (by scanning assistant
//     messages' ToolCalls).
//  3. Keep the most recent KeepRecent IDs; mark the rest for clearing.
//  4. Replace the Content of tool messages whose ToolCallID is marked with
//     TIME_BASED_MC_CLEARED_MESSAGE.
//
// This function operates in-memory only; it does NOT persist changes to the DB.
// The original messages slice is NOT modified — a new slice is returned.
func microcompactMessages(messages []*turnagent.Message, cfg MicrocompactConfig) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	// 1. Find the last assistant message and check the time gap.
	lastAssistantTime := findLastAssistantTime(messages)
	if lastAssistantTime.IsZero() {
		return messages
	}

	gapMinutes := time.Since(lastAssistantTime).Minutes()
	threshold := float64(cfg.GapThresholdMinutes)
	if threshold <= 0 {
		threshold = 60
	}
	if gapMinutes < threshold {
		return messages
	}

	// 2. Collect all compactable tool call IDs in order.
	compactableIDs := collectCompactableToolCallIDs(messages)
	if len(compactableIDs) == 0 {
		return messages
	}

	// 3. Determine which IDs to keep (most recent N).
	keepRecent := cfg.KeepRecent
	if keepRecent < 1 {
		keepRecent = 1
	}
	if len(compactableIDs) <= keepRecent {
		return messages
	}

	keepSet := make(map[string]struct{}, keepRecent)
	for _, id := range compactableIDs[len(compactableIDs)-keepRecent:] {
		keepSet[id] = struct{}{}
	}

	// 4. Build a new slice with cleared content for non-kept tool results.
	result := make([]*turnagent.Message, len(messages))
	for i, msg := range messages {
		if msg.Role == turnagent.RoleTool && msg.ToolCallID != "" {
			if _, kept := keepSet[msg.ToolCallID]; !kept {
				result[i] = &turnagent.Message{
					Role:       msg.Role,
					Content:    TIME_BASED_MC_CLEARED_MESSAGE,
					ToolName:   msg.ToolName,
					ToolCallID: msg.ToolCallID,
					CreatedAt:  msg.CreatedAt,
				}
				continue
			}
		}
		result[i] = msg
	}

	return result
}

// findLastAssistantTime returns the CreatedAt of the last assistant message
// in the slice, or the zero time if none is found.
func findLastAssistantTime(messages []*turnagent.Message) time.Time {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == turnagent.RoleAssistant && !messages[i].CreatedAt.IsZero() {
			return messages[i].CreatedAt
		}
	}
	return time.Time{}
}

// collectCompactableToolCallIDs scans the messages in order and collects the
// IDs of tool calls whose tool name is in COMPACTABLE_TOOLS. The IDs are
// returned in the order they appear (earliest first).
func collectCompactableToolCallIDs(messages []*turnagent.Message) []string {
	var ids []string
	for _, msg := range messages {
		if msg.Role == turnagent.RoleAssistant {
			for _, tc := range msg.ToolCalls {
				if COMPACTABLE_TOOLS[tc.Name] {
					ids = append(ids, tc.ID)
				}
			}
		}
	}
	return ids
}
