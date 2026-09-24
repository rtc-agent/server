package agent

// =============================================================================
// Response-level grouping for assistant messages
// =============================================================================
//
// groupAssistantByResponse and its helpers fix the structural mismatch between
// DB-loaded messages (interleaved assistant-tool pattern, one tool_call per
// assistant message) and in-memory messages (single assistant with multiple
// tool_calls per LLM response).
//
// The grouping uses three signals to detect LLM response boundaries within
// each TurnID group:
//  1. AbsorbedThinking marker (structural, primary for new data)
//  2. Time gap > threshold (fallback for interleaved patterns and legacy data)
//  3. Thinking-only assistant messages (DEFENSIVE: never triggers in practice)

import (
	"context"
	"sort"
	"time"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// responseBoundaryThreshold is the minimum time gap between consecutive
// assistant messages to consider them from different LLM responses.
// Used as a fallback for legacy data without structural markers.
//
// Observed data:
//   - Same response: 0-14ms gap
//   - Different responses: 1-2s gap
//
// 500ms provides a large safety margin on both sides.
const responseBoundaryThreshold = 500 * time.Millisecond

// groupAssistantByResponse groups assistant messages by LLM response within
// each TurnID group, merging assistant messages that belong to the same
// response and keeping messages from different responses separate.
//
// Problem:
//   - Same TurnID may contain multiple LLM responses (ReAct loop iterations)
//   - Each toolcall_input is stored as a separate DB message, producing
//     interleaved assistant-tool structure: assistant([A])->tool([rA])->assistant([B])->tool([rB])
//   - These assistant messages may belong to the SAME response (batch pattern)
//     or DIFFERENT responses (interleaved pattern with separate LLM calls)
//
// Solution:
//  1. Group consecutive messages by TurnID
//  2. Within each TurnID group, detect LLM response boundaries:
//     a. AbsorbedThinking marker -> new response (structural signal, primary)
//     b. Time gap > threshold -> new response (fallback for interleaved/legacy)
//     c. Thinking-only messages -> new response (DEFENSIVE: never triggers in
//     practice because mergeAssistantMessages always absorbs thinking into
//     the next assistant; kept as a safety net)
//  3. Within each response segment, merge all assistant messages into one
//  4. Collect tool messages and place after the merged assistant
//
// This produces the same structure as the in-memory ReAct loop path.
func groupAssistantByResponse(ctx context.Context, logger turnagent.Logger, messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) <= 1 {
		return messages
	}

	// Phase 1: Group by TurnID.
	type turnGroup struct {
		turnID   string
		messages []*turnagent.Message
	}

	var groups []turnGroup
	var current *turnGroup

	flush := func() {
		if current != nil && len(current.messages) > 0 {
			groups = append(groups, *current)
			current = nil
		}
	}

	for _, msg := range messages {
		if msg == nil {
			continue
		}

		// User/system messages are natural boundaries — not part of any turn group.
		if msg.Role != turnagent.RoleAssistant && msg.Role != turnagent.RoleTool {
			flush()
			groups = append(groups, turnGroup{messages: []*turnagent.Message{msg}})
			continue
		}

		// Assistant or tool message.
		turnID := msg.TurnID
		if turnID == "" {
			// Legacy message without TurnID — standalone.
			flush()
			groups = append(groups, turnGroup{messages: []*turnagent.Message{msg}})
			continue
		}

		if current == nil || current.turnID != turnID {
			flush()
			current = &turnGroup{turnID: turnID}
		}
		current.messages = append(current.messages, msg)
	}
	flush()

	// Phase 2: Within each turn group, detect response boundaries and merge.
	result := make([]*turnagent.Message, 0, len(messages))
	totalSegments := 0
	absorbedThinkingCount := 0
	timeGapCount := 0

	for _, group := range groups {
		if len(group.messages) <= 1 {
			result = append(result, group.messages...)
			continue
		}

		// Check if there are multiple assistant messages to potentially merge.
		assistantCount := 0
		for _, msg := range group.messages {
			if msg.Role == turnagent.RoleAssistant {
				assistantCount++
			}
		}
		if assistantCount <= 1 {
			result = append(result, group.messages...)
			continue
		}

		// Segment by response boundaries.
		segments, signalCounts := segmentByResponseBoundaries(group.messages)

		// Track statistics for debug logging.
		totalSegments += len(segments)
		absorbedThinkingCount += signalCounts.absorbedThinking
		timeGapCount += signalCounts.timeGap

		// Merge within each segment.
		for _, segment := range segments {
			mergedAssistant, toolMsgs := mergeSegment(segment)
			if mergedAssistant != nil {
				result = append(result, mergedAssistant)
			}
			result = append(result, toolMsgs...)
		}
	}

	// Debug logging for diagnostics (production: default off).
	if logger != nil {
		logger.Debug(ctx, "groupAssistantByResponse.summary", map[string]any{
			"input_count":       len(messages),
			"output_count":      len(result),
			"turn_groups":       len(groups),
			"response_segments": totalSegments,
			"boundaries_by_signal": map[string]int{
				"absorbed_thinking": absorbedThinkingCount,
				"time_gap":          timeGapCount,
			},
		})
	}

	return result
}

// signalCounts tracks how many response boundaries were detected by each signal.
type signalCounts struct {
	absorbedThinking int
	timeGap          int
}

// segmentByResponseBoundaries splits messages into segments, where each
// segment represents one LLM response.
//
// Boundary detection uses three signals (ordered by priority):
//  1. AbsorbedThinking marker (structural, high confidence, primary for new data)
//  2. Time gap > threshold (fallback for interleaved patterns and legacy data)
//  3. Thinking-only assistant messages (DEFENSIVE: never triggers because
//     mergeAssistantMessages always absorbs thinking into the next assistant;
//     kept as a safety net in case upstream behavior changes)
func segmentByResponseBoundaries(messages []*turnagent.Message) ([][]*turnagent.Message, signalCounts) {
	if len(messages) == 0 {
		return nil, signalCounts{}
	}

	var segments [][]*turnagent.Message
	var currentSegment []*turnagent.Message
	var lastAssistantTime time.Time
	var counts signalCounts

	for _, msg := range messages {
		if msg.Role == turnagent.RoleAssistant {
			signal := boundarySignal(msg, lastAssistantTime)
			if signal != signalNone && len(currentSegment) > 0 {
				// Start a new segment.
				segments = append(segments, currentSegment)
				currentSegment = nil
				switch signal {
				case signalAbsorbedThinking:
					counts.absorbedThinking++
				case signalTimeGap:
					counts.timeGap++
				}
			}
			lastAssistantTime = msg.CreatedAt
		}
		currentSegment = append(currentSegment, msg)
	}

	if len(currentSegment) > 0 {
		segments = append(segments, currentSegment)
	}

	return segments, counts
}

// boundarySignalKind identifies which signal indicated a response boundary.
type boundarySignalKind int

const (
	signalNone boundarySignalKind = iota
	signalAbsorbedThinking
	signalTimeGap
	signalThinkingOnly // DEFENSIVE: never triggers in practice.
)

// isResponseBoundary determines if the given assistant message starts a
// new LLM response (as opposed to continuing the previous one).
//
// Three signals are used (ordered by practical importance):
//  1. AbsorbedThinking marker: assistant with Extra[AbsorbedThinking]=true
//     -> This assistant had thinking merged into it by mergeAssistantMessages.
//     The thinking came from the same LLM response, indicating this is the
//     FIRST message of a new response (thinking is always produced first).
//     This is the primary structural signal for batch-pattern data.
//  2. Time gap: CreatedAt gap > responseBoundaryThreshold
//     -> Handles interleaved patterns (where tool execution creates time gaps
//     between LLM responses) and legacy data without structural markers.
//     Same-response messages are created within ~14ms; different-response
//     messages are separated by 1-2s (tool execution + next LLM call).
//  3. Thinking-only: assistant with ReasoningContent but no Content/ToolCalls
//     -> DEFENSIVE CODE. mergeAssistantMessages always absorbs thinking into
//     the next assistant, so thinking-only messages never survive to this
//     point. Kept as a safety net in case upstream behavior changes.
func isResponseBoundary(msg *turnagent.Message, lastAssistantTime time.Time) bool {
	return boundarySignal(msg, lastAssistantTime) != signalNone
}

// boundarySignal returns the highest-priority signal that indicates a response
// boundary. Returns signalNone if no boundary is detected.
//
// Signal priority: AbsorbedThinking > TimeGap > ThinkingOnly.
// AbsorbedThinking and TimeGap are the active signals; ThinkingOnly is
// defensive code kept as a safety net.
func boundarySignal(msg *turnagent.Message, lastAssistantTime time.Time) boundarySignalKind {
	// Signal 1 (DEFENSIVE): Thinking-only message.
	// In practice, mergeAssistantMessages always absorbs thinking into the next
	// assistant message, so thinking-only messages never survive to this point.
	// This signal is kept as a safety net in case upstream behavior changes.
	// Currently, Signal 2 (AbsorbedThinking) handles all thinking-related boundaries.
	if msg.ReasoningContent != "" && msg.Content == "" && len(msg.ToolCalls) == 0 {
		return signalThinkingOnly
	}

	// Signal 2: AbsorbedThinking marker (PRIMARY structural signal).
	if msg.Extra != nil {
		if _, ok := msg.Extra[turnagent.ExtraKeyAbsorbedThinking]; ok {
			return signalAbsorbedThinking
		}
	}

	// Signal 3: Time gap (fallback for interleaved patterns and legacy data).
	if !lastAssistantTime.IsZero() && !msg.CreatedAt.IsZero() {
		gap := msg.CreatedAt.Sub(lastAssistantTime)
		if gap > responseBoundaryThreshold {
			return signalTimeGap
		}
	}

	return signalNone
}

// mergeSegment merges all assistant messages within a response segment
// into a single message, and collects tool messages.
func mergeSegment(segment []*turnagent.Message) (*turnagent.Message, []*turnagent.Message) {
	var mergedAssistant *turnagent.Message
	var toolMsgs []*turnagent.Message

	for _, msg := range segment {
		switch msg.Role {
		case turnagent.RoleAssistant:
			if mergedAssistant == nil {
				// First assistant — shallow copy to avoid mutation.
				copied := *msg
				if len(msg.ToolCalls) > 0 {
					copied.ToolCalls = make([]turnagent.ToolCall, len(msg.ToolCalls))
					copy(copied.ToolCalls, msg.ToolCalls)
				}
				if msg.Extra != nil {
					copied.Extra = make(map[string]any, len(msg.Extra))
					for k, v := range msg.Extra {
						copied.Extra[k] = v
					}
				}
				mergedAssistant = &copied
			} else {
				// Subsequent assistant — merge into the first.
				mergeGroupedAssistantContent(mergedAssistant, msg)
			}
		case turnagent.RoleTool:
			toolMsgs = append(toolMsgs, msg)
		}
	}

	// Sort tool_calls by (name, arguments) for deterministic ordering.
	// This matches the behavior of normalizeToolCallOrdering in the
	// merge_assistant_middleware, ensuring consistency between the DB
	// path and the in-memory path.
	if mergedAssistant != nil && len(mergedAssistant.ToolCalls) > 1 {
		sort.SliceStable(mergedAssistant.ToolCalls, func(i, j int) bool {
			if mergedAssistant.ToolCalls[i].Name != mergedAssistant.ToolCalls[j].Name {
				return mergedAssistant.ToolCalls[i].Name < mergedAssistant.ToolCalls[j].Name
			}
			return mergedAssistant.ToolCalls[i].Arguments < mergedAssistant.ToolCalls[j].Arguments
		})
	}

	// After merging, clean up the AbsorbedThinking marker from the merged assistant.
	// The marker has served its purpose (response boundary detection) and should not
	// propagate to downstream pipelines or the LLM adapter.
	if mergedAssistant != nil && mergedAssistant.Extra != nil {
		delete(mergedAssistant.Extra, turnagent.ExtraKeyAbsorbedThinking)
	}

	return mergedAssistant, toolMsgs
}

// mergeGroupedAssistantContent merges src assistant message into dst.
// Used by mergeSegment to combine assistant messages from the same LLM response.
//
// Merge semantics:
//   - Content: joined with "\n"
//   - ToolCalls: appended
//   - ReasoningContent: joined with "\n"
//   - Extra: shallow merge (src keys overwrite dst keys)
//   - TokenUsage: keep dst's (first message's)
//   - CreatedAt: keep dst's (first message's)
func mergeGroupedAssistantContent(dst, src *turnagent.Message) {
	if src.Content != "" {
		dst.Content = joinContent(dst.Content, src.Content)
	}
	if len(src.ToolCalls) > 0 {
		dst.ToolCalls = append(dst.ToolCalls, src.ToolCalls...)
	}
	if src.ReasoningContent != "" {
		dst.ReasoningContent = joinContent(dst.ReasoningContent, src.ReasoningContent)
	}
	if src.Extra != nil {
		if dst.Extra == nil {
			dst.Extra = make(map[string]any, len(src.Extra))
		}
		for k, v := range src.Extra {
			dst.Extra[k] = v
		}
	}
}
