package agent

import (
	"context"
	"fmt"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Message Normalizer — Defensive LLM Message Sanitization
// =============================================================================
//
// normalizeMessagesForLLM ensures the message sequence conforms to the
// Anthropic Messages API structural invariants before messages are sent to
// the LLM. This is a defensive layer that prevents future code changes from
// introducing structural violations that would cause API errors.
//
// Pipeline order (each step feeds the next):
//  1. Extract system messages to the leading position.
//  2. Repair tool call/result pairing (drop orphaned entries).
//  3. Merge consecutive same-role user/assistant messages ("\n" join).
//  4. Validate the final sequence for structural correctness.
//
// Repair runs before merge because dropping an intermediate assistant/tool
// message during repair can create new consecutive same-role pairs that
// merge must absorb.
//
// The normalizer runs as the final step in the loadMessages pipeline,
// after all content injection (attachments, commands, scenarios) is complete.

// normalizeMessagesForLLM reshapes the message sequence to satisfy LLM API
// requirements. It performs four transformations in order:
//
//  1. Extract system messages to the leading position.
//  2. Repair tool call/result pairing (drop orphaned entries).
//  3. Merge consecutive same-role user/assistant messages (runs after repair
//     because repair can create new consecutive same-role adjacencies by
//     dropping an intermediate assistant or tool message).
//  4. Validate the final sequence for structural correctness.
//
// Anomalies are logged via h.logger.Info for observability.
// Returns an error only for unrecoverable structural issues.
func (h *helpers) normalizeMessagesForLLM(
	ctx context.Context,
	messages []*turnagent.Message,
) ([]*turnagent.Message, error) {
	if len(messages) == 0 {
		return messages, nil
	}

	originalLen := len(messages)

	// Step 1: Extract system messages to the leading position.
	// Future code changes might accidentally insert system messages into
	// the conversation body; this ensures they are always at the front.
	messages = extractSystemMessages(messages)

	// Step 2: Repair tool call/result pairing.
	// Removes orphaned tool results (no matching assistant call) and
	// assistant tool calls with no matching results. Runs BEFORE merge
	// because dropping messages can create new consecutive same-role
	// pairs (e.g., user→assistant(tool_call)→user becomes user→user
	// when the assistant is dropped).
	messages = repairToolPairing(messages)

	// Step 3: Merge consecutive same-role user/assistant messages.
	// Joins Content and ReasoningContent with "\n", preserving the first
	// message's metadata (CreatedAt, TokenUsage, ToolCalls).
	// Runs after repair to absorb any new consecutive pairs repair created.
	messages = mergeConsecutiveSameRole(messages)

	// Step 4: Validate the final sequence.
	// If repair+merge succeeded, validation should pass. Errors here indicate
	// a structural issue that repair could not fix — a bug worth investigating.
	if err := validateMessageSequence(messages); err != nil {
		h.logger.Info(ctx, "normalizeMessagesForLLM.sequence_invalid", map[string]any{
			"error": err.Error(),
		})
		return nil, fmt.Errorf("normalizeMessagesForLLM: %w", err)
	}

	// Log normalization summary for observability. Only log when changes
	// were actually made to avoid noise in the common (no-op) case.
	if len(messages) != originalLen {
		h.logger.Info(ctx, "normalizeMessagesForLLM.applied", map[string]any{
			"before": originalLen,
			"after":  len(messages),
		})
	}

	return messages, nil
}

// -----------------------------------------------------------------------------
// System message extraction
// -----------------------------------------------------------------------------

// extractSystemMessages pulls all system-role messages to the front of the
// slice, preserving relative order within both system and non-system groups.
//
// If no system messages are present, the original slice is returned as-is
// (no allocation).
func extractSystemMessages(messages []*turnagent.Message) []*turnagent.Message {
	var systemMsgs []*turnagent.Message
	var otherMsgs []*turnagent.Message

	for _, msg := range messages {
		if msg.Role == turnagent.RoleSystem {
			systemMsgs = append(systemMsgs, msg)
		} else {
			otherMsgs = append(otherMsgs, msg)
		}
	}

	// No system messages: nothing to do.
	if len(systemMsgs) == 0 {
		return messages
	}

	return append(systemMsgs, otherMsgs...)
}

// -----------------------------------------------------------------------------
// Consecutive same-role merging
// -----------------------------------------------------------------------------

// mergeConsecutiveSameRole merges adjacent messages with the same role
// (user or assistant) into a single message. Content fields are joined
// with "\n". Tool and system messages are never merged — each carries
// identity that must be preserved (ToolCallID for pairing, independent
// prompt boundaries for system).
//
// The merged message retains the first message's metadata: CreatedAt,
// TokenUsage, ToolCalls, Extra, etc.
//
// NOTE: The first message of each merged group is shallow-copied before
// mutation to avoid modifying the caller's original message objects.
// Non-merged messages are NOT copied (they pass through as-is).
func mergeConsecutiveSameRole(messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) <= 1 {
		return messages
	}

	result := make([]*turnagent.Message, 0, len(messages))
	// mergedIdx tracks the result index of the current merge target.
	// -1 means the last result entry has NOT been copied yet.
	mergedIdx := -1

	for i := 0; i < len(messages); i++ {
		curr := messages[i]

		if len(result) > 0 {
			prev := result[len(result)-1]
			if curr.Role == prev.Role &&
				(curr.Role == turnagent.RoleUser || curr.Role == turnagent.RoleAssistant) {
				// Copy-on-first-merge: the first time we merge INTO prev,
				// replace it with a shallow copy to avoid mutating the caller's
				// original message object.
				if mergedIdx != len(result)-1 {
					copied := *prev
					result[len(result)-1] = &copied
					prev = &copied
					mergedIdx = len(result) - 1
				}
				prev.Content = joinContent(prev.Content, curr.Content)
				prev.ReasoningContent = joinContent(prev.ReasoningContent, curr.ReasoningContent)
				continue
			}
		}

		// Reset merge tracker: new group starts.
		mergedIdx = -1
		result = append(result, curr)
	}

	return result
}

// joinContent concatenates two content strings with "\n".
// Empty inputs are handled gracefully — no leading/trailing newlines.
func joinContent(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}

// -----------------------------------------------------------------------------
// Message sequence validation
// -----------------------------------------------------------------------------

// validateMessageSequence checks the structural correctness of a message
// sequence for the Anthropic Messages API.
//
// Rules enforced:
//  1. System messages must occupy the leading position (no interleaving).
//  2. Non-system messages follow valid turn structure:
//     - The first non-system message may be user or assistant (prefill).
//     - User must be followed by assistant or tool (tool-use continuation).
//     - Assistant without tool calls must be followed by user.
//     - Assistant with tool calls must be followed by tool messages.
//     - Tool messages must be followed by assistant (model processes results)
//     or user (human interrupts the tool-use chain).
//  3. Tool pairing is structurally sound (delegated to validateToolPairing).
func validateMessageSequence(messages []*turnagent.Message) error {
	if len(messages) == 0 {
		return nil
	}

	// Rule 1: System messages must be leading.
	inConversation := false
	for i, msg := range messages {
		if msg.Role == turnagent.RoleSystem {
			if inConversation {
				return fmt.Errorf(
					"system message at position %d after conversation start", i)
			}
		} else {
			inConversation = true
		}
	}

	// Find the first non-system message index.
	convStart := 0
	for convStart < len(messages) && messages[convStart].Role == turnagent.RoleSystem {
		convStart++
	}

	// Empty conversation (all system or no messages).
	if convStart >= len(messages) {
		return nil
	}

	// Rule 2: First non-system message must be user or assistant.
	first := messages[convStart]
	if first.Role != turnagent.RoleUser && first.Role != turnagent.RoleAssistant {
		return fmt.Errorf(
			"first non-system message at position %d has invalid role %q",
			convStart, first.Role)
	}

	// Rule 3: Validate role transitions in the conversation body.
	if err := validateRoleTransitions(messages, convStart); err != nil {
		return err
	}

	// Rule 4: Tool pairing integrity.
	return validateToolPairing(messages)
}

// validateRoleTransitions checks that each role transition in the conversation
// body follows the Anthropic Messages API contract.
func validateRoleTransitions(messages []*turnagent.Message, convStart int) error {
	for i := convStart; i < len(messages)-1; i++ {
		curr := messages[i]
		next := messages[i+1]

		switch curr.Role {
		case turnagent.RoleUser:
			if next.Role != turnagent.RoleAssistant && next.Role != turnagent.RoleTool {
				return fmt.Errorf(
					"user message at position %d followed by %s (expected assistant or tool)",
					i, next.Role)
			}

		case turnagent.RoleAssistant:
			if len(curr.ToolCalls) > 0 {
				if next.Role != turnagent.RoleTool {
					return fmt.Errorf(
						"assistant with tool_calls at position %d not followed by tool",
						i)
				}
			} else if next.Role != turnagent.RoleUser {
				return fmt.Errorf(
					"assistant at position %d followed by %s (expected user)",
					i, next.Role)
			}

		case turnagent.RoleTool:
			if next.Role != turnagent.RoleAssistant && next.Role != turnagent.RoleUser {
				return fmt.Errorf(
					"tool message at position %d followed by %s (expected assistant or user)",
					i, next.Role)
			}
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Tool pairing validation and repair
// -----------------------------------------------------------------------------

// validateToolPairing checks that every tool result message has a matching
// assistant tool call, and every assistant tool call has a matching result.
//
// Uses a counter-based approach: pending tracks the number of tool calls
// awaiting results. At each tool message, one pending call is consumed.
// At the end, pending must be zero.
func validateToolPairing(messages []*turnagent.Message) error {
	pending := 0

	for i, msg := range messages {
		switch msg.Role {
		case turnagent.RoleAssistant:
			pending += len(msg.ToolCalls)

		case turnagent.RoleTool:
			if pending <= 0 {
				return fmt.Errorf(
					"tool message at position %d without matching assistant tool_call",
					i)
			}
			pending--
		}
	}

	if pending > 0 {
		return fmt.Errorf(
			"%d assistant tool_call(s) without matching tool result", pending)
	}

	return nil
}

// repairToolPairing fixes broken tool call/result pairing by removing
// orphaned entries that cannot be matched.
//
// Repair strategy (two-pass):
//
//	Pass 1 — identify which assistant messages to keep and collect their
//	         tool call IDs into a "valid" set. Assistants with unmatched
//	         calls are either stripped (if they have text content) or
//	         dropped entirely.
//	Pass 2 — filter tool results: keep only those whose ToolCallID appears
//	         in the valid set. This prevents orphaned tool results when
//	         the calling assistant was dropped in Pass 1.
//
// This is a defensive measure — under normal operation, the pipeline
// produces well-paired messages. Repair exists to absorb edge cases
// from future code changes or data corruption.
func repairToolPairing(messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	// Build a set of tool call IDs that have at least one result message.
	matchedCallIDs := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role == turnagent.RoleTool && msg.ToolCallID != "" {
			matchedCallIDs[msg.ToolCallID] = true
		}
	}

	// Pass 1: decide which assistant messages to keep and collect valid
	// tool call IDs. Only tool calls from KEPT assistant messages (with
	// calls intact) are considered valid.
	validCallIDs := make(map[string]bool)
	var kept []*turnagent.Message

	for _, msg := range messages {
		if msg.Role != turnagent.RoleAssistant || len(msg.ToolCalls) == 0 {
			// Non-assistant or assistant without calls: keep unconditionally.
			kept = append(kept, msg)
			continue
		}

		// Check if ALL tool calls have matching results.
		allMatched := true
		for _, tc := range msg.ToolCalls {
			if !matchedCallIDs[tc.ID] {
				allMatched = false
				break
			}
		}

		if allMatched {
			// All calls matched: keep the message, register call IDs as valid.
			kept = append(kept, msg)
			for _, tc := range msg.ToolCalls {
				validCallIDs[tc.ID] = true
			}
		} else if msg.Content != "" || msg.ReasoningContent != "" {
			// Partial match with text content: keep message but strip ALL calls.
			// The text is valuable; the calls are broken. No valid IDs registered.
			cleaned := *msg
			cleaned.ToolCalls = nil
			kept = append(kept, &cleaned)
		}
		// else: pure tool-call message with unmatched calls, drop entirely.
	}

	// Pass 2: filter tool results using validCallIDs.
	// Only keep tool results whose calling assistant was kept with calls intact.
	// Track used IDs to handle (unlikely) duplicate results.
	usedToolIDs := make(map[string]bool)
	result := make([]*turnagent.Message, 0, len(kept))
	for _, msg := range kept {
		if msg.Role == turnagent.RoleTool {
			if msg.ToolCallID != "" && validCallIDs[msg.ToolCallID] && !usedToolIDs[msg.ToolCallID] {
				usedToolIDs[msg.ToolCallID] = true
				result = append(result, msg)
			}
			// else: orphan result (assistant dropped or stripped), skip.
			continue
		}
		result = append(result, msg)
	}

	return result
}
