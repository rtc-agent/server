package primitives

import (
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/pkg/protocol"
)

// TextContentData builds a ContentData of plain-text type.
func TextContentData(text string) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeText,
		Data: text,
	}, nil
}

// MarkdownContentData builds a ContentData of Markdown type.
func MarkdownContentData(markdown string) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeMarkdown,
		Data: markdown,
	}, nil
}

// ThinkingContentData builds a ContentData carrying the LLM's reasoning trace.
func ThinkingContentData(thinking string) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeThinking,
		Data: thinking,
	}, nil
}

// SummaryItem represents a single message in a conversation summary.
type SummaryItem struct {
	Role    string
	Content string
}

// SummaryMetadata captures metadata about a compression operation.
type SummaryMetadata struct {
	// TokensBefore is the context token count before compression.
	TokensBefore int `json:"tokens_before"`
	// TokensAfter is the estimated context token count after compression.
	TokensAfter int `json:"tokens_after"`
	// DurationMs is the wall-clock cost of the compression in milliseconds.
	DurationMs int64 `json:"duration_ms"`
	// Mode is the compression mode: "full" or "partial".
	Mode string `json:"mode"`
	// SessionMemoryUsed indicates whether session memory was used (zero-cost compression).
	SessionMemoryUsed bool `json:"session_memory_used,omitempty"`
}

// SummaryContent is the full summary content structure (items + metadata).
// Backward compatibility: legacy rows store Data as []SummaryItem (JSON array);
// new rows store SummaryContent (JSON object).
type SummaryContent struct {
	Items    []SummaryItem    `json:"items"`
	Metadata *SummaryMetadata `json:"metadata,omitempty"`
}

// SummaryContentData builds a ContentData of context-summary type.
func SummaryContentData(summaryList []SummaryItem) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeSummary,
		Data: summaryList,
	}, nil
}

// SummaryContentDataWithMetadata builds a summary ContentData carrying metadata.
func SummaryContentDataWithMetadata(
	items []SummaryItem,
	metadata *SummaryMetadata,
) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeSummary,
		Data: SummaryContent{
			Items:    items,
			Metadata: metadata,
		},
	}, nil
}

// SerializeContentData serializes a ContentData to a JSON string
// (for persistence in the database Content column).
func SerializeContentData(cd protocol.ContentData) (string, error) {
	jsonBytes, err := json.Marshal(cd)
	if err != nil {
		return "", fmt.Errorf("serialize content data: %w", err)
	}
	return string(jsonBytes), nil
}

// ParseContentData parses a ContentData from a JSON string.
func ParseContentData(content string) (protocol.ContentData, error) {
	if content == "" {
		return protocol.ContentData{}, nil
	}
	var cd protocol.ContentData
	if err := json.Unmarshal([]byte(content), &cd); err != nil {
		return protocol.ContentData{}, fmt.Errorf("parse content data: %w", err)
	}
	return cd, nil
}

// ParseContentDataToolCall converts an arbitrary value into a protocol.ToolCall
// by round-tripping through JSON serialization.
func ParseContentDataToolCall(data any) (protocol.ToolCall, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return protocol.ToolCall{}, fmt.Errorf("marshal tool call data: %w", err)
	}
	v := &protocol.ToolCall{}
	e := json.Unmarshal(bytes, v)
	return *v, e
}

// ContentDataBytes re-serializes ContentData.Data (any) to JSON bytes.
func ContentDataBytes(data any) ([]byte, error) {
	return json.Marshal(data)
}

// ParseUserMessageContent parses a UserMessageContent from an arbitrary value.
// The input arrives as map[string]interface{} after JSON deserialization and
// requires a second conversion pass through JSON round-tripping.
func ParseUserMessageContent(data any) (protocol.UserMessageContent, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return protocol.UserMessageContent{}, fmt.Errorf("marshal user message data: %w", err)
	}
	var umc protocol.UserMessageContent
	if err := json.Unmarshal(bytes, &umc); err != nil {
		return protocol.UserMessageContent{}, fmt.Errorf("unmarshal user message content: %w", err)
	}
	return umc, nil
}

// UserMessageContentData builds a ContentData of user-message type.
func UserMessageContentData(text string, scenarios []protocol.ScenarioRef) (protocol.ContentData, error) {
	content := protocol.UserMessageContent{
		Text: text,
	}
	if len(scenarios) > 0 {
		content.Scenarios = &scenarios
	}
	return protocol.ContentData{
		Type: protocol.ContentTypeUserMessage,
		Data: content,
	}, nil
}

// ContentDataString extracts ContentData.Data (any) as a string.
// JSON strings are returned as-is (after unquoting); other types are
// serialized to a JSON string.
func ContentDataString(data any) (string, error) {
	if s, ok := data.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	// If the serialization produced a JSON string (quoted), unmarshal
	// to recover the original unquoted value.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return s, nil
	}
	return string(b), nil
}
