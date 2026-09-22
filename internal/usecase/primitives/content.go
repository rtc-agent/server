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

// ToolCallData is the DB storage representation of a ToolCall.
// Unlike protocol.ToolCall (where Input/Output are strings), ToolCallData uses
// json.RawMessage so that the JSON values are embedded directly in the parent
// ContentData envelope — avoiding the extra escape layer that string fields
// incur when json.Marshal serialises the parent.
//
// DB comparison (same tool result):
//
//	Before (protocol.ToolCall): "input":"{\"action\":\"eval\"}" (escaped)
//	After  (ToolCallData):      "input":{"action":"eval"}       (raw JSON)
type ToolCallData struct {
	Id       string          `json:"id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
	Output   json.RawMessage `json:"output,omitempty"`
	Status   *string         `json:"status,omitempty"`
}

// SerializeContentData serializes a ContentData to a JSON string
// (for persistence in the database Content column).
//
// Special case: when Data is a protocol.ToolCall, it is converted to
// ToolCallData first so that Input/Output are stored as raw JSON objects
// instead of escaped JSON strings. See ToolCallData for details.
func SerializeContentData(cd protocol.ContentData) (string, error) {
	// Convert protocol.ToolCall → ToolCallData for clean DB storage.
	if tc, ok := cd.Data.(protocol.ToolCall); ok {
		storage, convErr := toolCallToStorage(tc)
		if convErr != nil {
			return "", fmt.Errorf("convert tool call for storage: %w", convErr)
		}
		cd.Data = storage
	}

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
//
// Backward compatible: handles both old format (input/output as JSON strings)
// and new format (input/output as raw JSON objects stored by ToolCallData).
func ParseContentDataToolCall(data any) (protocol.ToolCall, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return protocol.ToolCall{}, fmt.Errorf("marshal tool call data: %w", err)
	}

	// Inspect the raw map to handle both string and object formats for
	// input/output. This is necessary because protocol.ToolCall uses string
	// fields, but new DB records store input/output as JSON objects.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bytes, &raw); err != nil {
		return protocol.ToolCall{}, fmt.Errorf("unmarshal tool call raw: %w", err)
	}

	tc := protocol.ToolCall{}

	// id
	if v, ok := raw["id"]; ok {
		_ = json.Unmarshal(v, &tc.Id)
	}
	// tool_name
	if v, ok := raw["tool_name"]; ok {
		_ = json.Unmarshal(v, &tc.ToolName)
	}
	// status
	if v, ok := raw["status"]; ok && len(v) > 0 && string(v) != "null" {
		var s string
		_ = json.Unmarshal(v, &s)
		tc.Status = &s
	}

	// input: old format = JSON string ("…"), new format = JSON object ({…})
	if v, ok := raw["input"]; ok && len(v) > 0 {
		trimmed := trimJSONNull(v)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			// Old format: JSON string → use as-is (it's already a JSON string).
			_ = json.Unmarshal(trimmed, &tc.Input)
		} else {
			// New format: JSON object → marshal to string for protocol.ToolCall.
			tc.Input = string(trimmed)
		}
	}

	// output: same dual-format handling
	if v, ok := raw["output"]; ok && len(v) > 0 {
		trimmed := trimJSONNull(v)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			var s string
			_ = json.Unmarshal(trimmed, &s)
			tc.Output = &s
		} else if len(trimmed) > 0 && string(trimmed) != "null" {
			s := string(trimmed)
			tc.Output = &s
		}
	}

	return tc, nil
}

// toolCallToStorage converts a protocol.ToolCall (string fields) to
// ToolCallData (json.RawMessage fields) for DB storage.
//
// The key transformation: JSON string values like `{"action":"eval"}` are
// unmarshalled into interface{} and re-marshalled, producing clean JSON
// objects instead of escaped JSON strings.
func toolCallToStorage(tc protocol.ToolCall) (ToolCallData, error) {
	s := ToolCallData{
		Id:       tc.Id,
		ToolName: tc.ToolName,
		Status:   tc.Status,
	}

	// Input: parse the JSON string into an object, then store as raw JSON.
	if tc.Input != "" {
		var inputObj any
		if err := json.Unmarshal([]byte(tc.Input), &inputObj); err != nil {
			// Not valid JSON — store as a plain string.
			s.Input, _ = json.Marshal(tc.Input)
		} else {
			var err error
			s.Input, err = json.Marshal(inputObj)
			if err != nil {
				return ToolCallData{}, fmt.Errorf("marshal input: %w", err)
			}
		}
	}

	// Output: same transformation.
	if tc.Output != nil && *tc.Output != "" {
		var outputObj any
		if err := json.Unmarshal([]byte(*tc.Output), &outputObj); err != nil {
			s.Output, _ = json.Marshal(*tc.Output)
		} else {
			var err error
			s.Output, err = json.Marshal(outputObj)
			if err != nil {
				return ToolCallData{}, fmt.Errorf("marshal output: %w", err)
			}
		}
	}

	return s, nil
}

// trimJSONNull strips a leading "null" literal from raw JSON bytes,
// returning an empty slice if the value is JSON null.
func trimJSONNull(v json.RawMessage) json.RawMessage {
	if string(v) == "null" {
		return nil
	}
	return v
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

// PromptContentData builds a ContentData of prompt type.
// Used for persisting system-level instructions (e.g., scenarios, goals) as messages.
func PromptContentData(name, title, prompt string) (protocol.ContentData, error) {
	return PromptContentDataWithRole(name, title, prompt, "")
}

// PromptContentDataWithRole builds a ContentData of prompt type with role override.
// Role controls how the prompt is injected into LLM context:
// - "" or "system": injected as system message (default)
// - "user": injected as user message (will be merged with consecutive user messages)
func PromptContentDataWithRole(name, title, prompt, role string) (protocol.ContentData, error) {
	content := protocol.PromptContent{
		Name:   name,
		Prompt: prompt,
	}
	if title != "" {
		content.Title = &title
	}
	if role != "" && role != "system" {
		r := protocol.PromptContentRole(role)
		content.Role = &r
	}
	return protocol.ContentData{
		Type: protocol.ContentTypePrompt,
		Data: content,
	}, nil
}

// ParsePromptContent parses a PromptContent from an arbitrary value.
// The input arrives as map[string]interface{} after JSON deserialization and
// requires a second conversion pass through JSON round-tripping.
func ParsePromptContent(data any) (protocol.PromptContent, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return protocol.PromptContent{}, fmt.Errorf("marshal prompt content: %w", err)
	}
	var pc protocol.PromptContent
	if err := json.Unmarshal(bytes, &pc); err != nil {
		return protocol.PromptContent{}, fmt.Errorf("unmarshal prompt content: %w", err)
	}
	return pc, nil
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
