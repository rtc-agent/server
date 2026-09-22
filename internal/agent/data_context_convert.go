package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Message conversion and filtering utilities
// =============================================================================
//
// This file contains helper functions for converting DB messages to turn-agent
// messages and filtering/merging them for LLM consumption.

// convertDBMessage converts a single model.Message to zero or more
// turnagent.Messages.
//
// A single DB message may produce multiple turnagent messages (e.g., a
// toolcall_input produces an assistant message with tool calls; a summary
// may expand into multiple messages).
//
// Returns (nil, nil) for unrecognized content types (silently skipped).
// Returns (nil, err) for parse errors — callers can log the error for
// observability while still skipping the unparseable message.
func convertDBMessage(msg *model.Message) ([]*turnagent.Message, error) {
	contentData, err := primitives.ParseContentData(msg.Content)
	if err != nil {
		return nil, fmt.Errorf("parse content data: %w", err)
	}

	// Build TokenUsage from DB fields (populated for assistant messages).
	var tokenUsage *turnagent.TokenUsage
	if msg.TotalTokens != nil {
		tokenUsage = &turnagent.TokenUsage{
			TotalTokens:  *msg.TotalTokens,
			InputTokens:  intDeref(msg.InputTokens),
			OutputTokens: intDeref(msg.OutputTokens),
		}
		if msg.CachedTokens != nil {
			tokenUsage.CachedTokens = *msg.CachedTokens
		}
		if msg.ReasoningTokens != nil {
			tokenUsage.ReasoningTokens = *msg.ReasoningTokens
		}
	}

	switch contentData.Type {
	case protocol.ContentTypeSummary:
		return convertSummaryContent(contentData.Data, msg.CreatedAt)

	case protocol.ContentTypeUserMessage:
		umc, err := primitives.ParseUserMessageContent(contentData.Data)
		if err != nil {
			return nil, fmt.Errorf("parse user message content: %w", err)
		}
		return []*turnagent.Message{{
			Role:       msg.Role,
			Content:    umc.Text,
			TokenUsage: tokenUsage,
			CreatedAt:  msg.CreatedAt,
		}}, nil

	case protocol.ContentTypeText, protocol.ContentTypeMarkdown:
		text, _ := primitives.ContentDataString(contentData.Data)
		// Sanitize leaked think tags from assistant message content.
		// This is a defensive measure against models (typically qwen3.7-plus via proxy)
		// that leak thinking tags into the text content field.
		if msg.Role == string(schema.Assistant) {
			text = sanitizeThinkTagLeak(text)
		}
		return []*turnagent.Message{{
			Role:       msg.Role,
			Content:    text,
			TokenUsage: tokenUsage,
			CreatedAt:  msg.CreatedAt,
		}}, nil

	case protocol.ContentTypeThinking:
		tk, _ := primitives.ContentDataString(contentData.Data)
		return []*turnagent.Message{{
			Role:             msg.Role,
			ReasoningContent: tk,
			CreatedAt:        msg.CreatedAt,
		}}, nil

	case protocol.ContentTypeToolCallInput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil, fmt.Errorf("parse tool call input: %w", err)
		}
		// Produce an assistant message with tool calls.
		return []*turnagent.Message{{
			Role: string(schema.Assistant),
			ToolCalls: []turnagent.ToolCall{{
				ID:        toolCall.Id,
				Name:      toolCall.ToolName,
				Arguments: toolCall.Input,
			}},
			CreatedAt: msg.CreatedAt,
		}}, nil

	case protocol.ContentTypeToolCallOutput:
		toolCall, err := primitives.ParseContentDataToolCall(contentData.Data)
		if err != nil {
			return nil, fmt.Errorf("parse tool call output: %w", err)
		}
		content := formatToolCallOutput(toolCall)
		return []*turnagent.Message{{
			Role:       string(schema.Tool),
			Content:    content,
			ToolName:   toolCall.ToolName,
			ToolCallID: toolCall.Id,
			CreatedAt:  msg.CreatedAt,
		}}, nil

	case protocol.ContentTypePrompt:
		// Prompt messages are handled separately by extractAndInjectPrompts.
		// Skip here to keep them out of the conversation history.
		return nil, nil

	default:
		// Unrecognized content type — silently skip (not a parse error).
		return nil, nil
	}
}

// intDeref safely dereferences a *int, returning 0 for nil.
func intDeref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// convertSummaryContent expands a summary content block into multiple messages.
// Supports both old format ([]SummaryItem) and new format (SummaryContent{items, metadata}).
func convertSummaryContent(data any, createdAt time.Time) ([]*turnagent.Message, error) {
	dataBytes, err := primitives.ContentDataBytes(data)
	if err != nil {
		return nil, fmt.Errorf("summary content data bytes: %w", err)
	}

	// Try new format first (SummaryContent with items and metadata)
	var content primitives.SummaryContent
	if err := json.Unmarshal(dataBytes, &content); err == nil && content.Items != nil {
		return buildMessagesFromSummaryItems(content.Items, createdAt), nil
	}

	// Fallback to old format ([]SummaryItem)
	var items []primitives.SummaryItem
	if err := json.Unmarshal(dataBytes, &items); err == nil {
		return buildMessagesFromSummaryItems(items, createdAt), nil
	}

	return nil, fmt.Errorf("summary content: neither new nor old format parsed")
}

// buildMessagesFromSummaryItems converts SummaryItem slice to turnagent messages.
// Marks the last message with ExtraKeySummaryBoundary for strategic cache breakpoints.
func buildMessagesFromSummaryItems(items []primitives.SummaryItem, createdAt time.Time) []*turnagent.Message {
	msgs := make([]*turnagent.Message, 0, len(items))
	for i, item := range items {
		msg := &turnagent.Message{
			Role:      item.Role,
			Content:   item.Content,
			CreatedAt: createdAt,
		}
		// Mark the last summary message as the summary boundary.
		// This is where strategic cache breakpoint bp1 is set (see setCacheBreakpoints).
		if i == len(items)-1 {
			if msg.Extra == nil {
				msg.Extra = make(map[string]any)
			}
			msg.Extra[turnagent.ExtraKeySummaryBoundary] = true
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// filterMeaninglessThinking removes thinking messages that contain only
// placeholder content (like "...\n" or "[no content]\n") which can appear
// after interrupt/resume due to eino checkpoint restoration issues.
//
// Instead of pattern-matching each variant, we use a simple length threshold:
// thinking content shorter than minThinkingLength (after trimming) is not
// useful reasoning — no real LLM reasoning fits in under 20 characters.
//
// This catches all known variants: "...", "[no content]", empty strings, etc.
//
// The filter only removes assistant messages that:
// 1. Have no Content or ToolCalls (pure thinking, no other output)
// 2. Have ReasoningContent shorter than minThinkingLength after trimming
func filterMeaninglessThinking(messages []*turnagent.Message) []*turnagent.Message {
	result := make([]*turnagent.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}

		// Only filter assistant messages with no content or tool calls.
		if msg.Role == turnagent.RoleAssistant &&
			msg.Content == "" &&
			len(msg.ToolCalls) == 0 {

			trimmed := strings.TrimSpace(msg.ReasoningContent)
			if utf8.RuneCountInString(trimmed) < minThinkingLength {
				// Skip this message (filter it out)
				continue
			}
		}

		result = append(result, msg)
	}
	return result
}

// minThinkingLength is the minimum character count (after trimming) for
// thinking content to be considered meaningful. Real LLM reasoning is
// always longer than this; shorter content is a placeholder artifact.
const minThinkingLength = 20

// thinkTagPattern matches a complete `<think>...</think>` block (case-insensitive,
// dot-all) that may leak into assistant text content during streaming (observed with
// qwen3.7-plus via proxy).
// See: https://github.com/cloudwego/eino-ext/issues/518, #767
var thinkTagPattern = regexp.MustCompile(`(?is)<think>.*?</think>\s*`)

// sanitizeThinkTagLeak removes complete `<think>...</think>` blocks from assistant text content.
// This is a defensive measure — the model (typically qwen3.7-plus via proxy)
// sometimes leaks thinking tags into the text content field, polluting the
// context for subsequent turns.
func sanitizeThinkTagLeak(content string) string {
	return thinkTagPattern.ReplaceAllString(content, "")
}

// mergeAssistantMessages merges consecutive thinking + text/tool messages from
// the same assistant turn into single messages.
//
// The streaming handler (handleStreamChunk) stores thinking and markdown as
// separate DB messages. When loaded back via convertDBMessage, a thinking-only
// message has Content="" and ReasoningContent set. The eino-ext Claude adapter
// reads thinking from Extra["_eino_claude_thinking"] (not ReasoningContent),
// so without Extra populated the message appears empty. The adapter's fallback
// then produces {"text": "", "type": "text"} — the empty content blocks seen
// in the LLM-API log.
//
// This function fixes that by:
//  1. Merging thinking -> text pairs: the thinking's ReasoningContent is moved
//     into the next assistant text message's ReasoningContent, and the
//     thinking-only message is dropped.
//  2. Dropping standalone thinking messages followed by a non-assistant message
//     (typically a tool result). Converting these to text would create an
//     orphan assistant{text} before the tool{result}, breaking the tool-call
//     pairing contract required by the Claude API.
//  3. Converting trailing thinking messages (last in the conversation, no
//     following message at all) to text so the content is preserved.
//
// This is an in-memory-only transformation; the DB is not modified.
func mergeAssistantMessages(messages []*turnagent.Message) []*turnagent.Message {
	if len(messages) == 0 {
		return messages
	}

	result := make([]*turnagent.Message, 0, len(messages))
	i := 0
	for i < len(messages) {
		msg := messages[i]

		// Skip nil entries (can occur when convertDBMessage returns nil).
		if msg == nil {
			i++
			continue
		}

		// Check for thinking-only assistant message: has ReasoningContent
		// but no Content, no ToolCalls.
		if msg.Role == turnagent.RoleAssistant &&
			msg.ReasoningContent != "" &&
			msg.Content == "" &&
			len(msg.ToolCalls) == 0 {

			// Look ahead for the next assistant text/tool message.
			next := i + 1
			// thinkingContent accumulates reasoning from consecutive thinking-only
			// messages without mutating the original objects.
			thinkingContent := msg.ReasoningContent
			for next < len(messages) &&
				messages[next].Role == turnagent.RoleAssistant &&
				messages[next].Content == "" &&
				messages[next].ReasoningContent != "" &&
				len(messages[next].ToolCalls) == 0 {
				// Accumulate reasoning from consecutive thinking-only messages.
				thinkingContent += "\n" + messages[next].ReasoningContent
				next++
			}

			if next < len(messages) && messages[next].Role == turnagent.RoleAssistant {
				// Merge: move thinking into the next message's ReasoningContent.
				// Shallow-copy to avoid mutating the caller's original message.
				copied := *messages[next]
				copied.ReasoningContent = thinkingContent
				// Drop the thinking-only message; advance past it.
				i = next
				msg = &copied
				// Fall through to append msg to result below.
			} else if next < len(messages) {
				// Followed by a non-assistant message (typically tool result).
				// Drop the thinking to avoid:
				//   1. Empty content block (adapter fallback: {text: ""})
				//   2. Orphan assistant{text} before tool{result} which
				//      breaks tool call pairing in the Claude API.
				// The reasoning is implicitly preserved in the tool call's
				// arguments (e.g., the file path the model decided to read).
				i++
				continue
			} else {
				// Trailing thinking (last message in conversation): convert to
				// text so the content is preserved as a text block.
				// Shallow-copy to avoid mutating the caller's original message.
				copied := *msg
				copied.Content = thinkingContent
				copied.ReasoningContent = ""
				msg = &copied
			}
		}

		result = append(result, msg)
		i++
	}
	return result
}
