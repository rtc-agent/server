package agent

import (
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Strategic Cache Breakpoints
// =============================================================================
//
// Strategic cache breakpoints protect stable content from invalidation caused
// by microcompact or tool result budget modifications. The key insight is that
// these modifications only affect messages after the summary boundary, so by
// setting a breakpoint at the summary, we ensure the prefix (system + attachments
// + summary) remains cacheable.
//
// Breakpoint allocation (Anthropic API limit: 4 per request):
//   - bp1: summary message (protects all stable content)
//   - bp2: last conversation message (protects latest context)
//   - bp3: last tool definition (set automatically by AutoCacheControl)
//   - Total: 2 manual + 1 auto = 3, leaving 1 spare

// setCacheBreakpoints sets strategic cache breakpoints to protect stable content
// from invalidation caused by microcompact or applyToolResultBudget modifications.
//
// IMPORTANT: This function must be called AFTER normalizeMessagesForLLM(), which
// reorders system messages (extracts them to the front). The summary boundary
// marker is set by buildMessagesFromSummaryItems and survives normalization.
//
// Breakpoint allocation strategy:
//   - bp1 (summary, TTL=1h): Set on the last summary message identified by
//     ExtraKeySummaryBoundary. Protects all stable content (system + attachments
//   - summary). Uses 1h TTL because summary content is long-lived.
//   - bp2 (last msg, TTL=5m): Set on the last user/assistant message. Protects
//     the latest conversation context. Uses 5m TTL (default).
//
// Why bp2 is necessary: AutoCacheControl detects any manual message breakpoint
// and skips its automatic breakpoint on the last message. We must explicitly set
// bp2 to ensure the last message is cached.
func (h *helpers) setCacheBreakpoints(msgs []*turnagent.Message) []*turnagent.Message {
	if len(msgs) == 0 {
		return msgs
	}

	// bp1: Set on the summary boundary message (search from the end to find
	// the most recent summary if multiple exist).
	for i := len(msgs) - 1; i >= 0; i-- {
		if isSummaryMessage(msgs[i]) {
			msgs[i].CacheBreakpoint = true
			msgs[i].CacheTTL = "1h" // Summary is stable, use long TTL
			break
		}
	}

	// bp2: Set on the last conversation message (user or assistant).
	// This ensures the latest context is cached, compensating for AutoCacheControl's
	// skip behavior when manual breakpoints are detected.
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == turnagent.RoleUser || msgs[i].Role == turnagent.RoleAssistant {
			msgs[i].CacheBreakpoint = true
			msgs[i].CacheTTL = "5m" // Latest context uses default TTL
			break
		}
	}

	return msgs
}

// isSummaryMessage checks if a message is marked as the summary boundary.
// Uses Extra[ExtraKeySummaryBoundary] rather than text detection, which is fragile
// (streaming path stores raw LLM output without wrapper text).
//
// All summary messages in the DB use Role="system", but we don't check Role here
// to keep the function focused on the marker alone.
func isSummaryMessage(m *turnagent.Message) bool {
	if m.Extra == nil {
		return false
	}
	isBoundary, _ := m.Extra[turnagent.ExtraKeySummaryBoundary].(bool)
	return isBoundary
}
