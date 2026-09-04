package primitives

import (
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/pkg/protocol"
)

// TextContentData 创建纯文本类型的 ContentData
func TextContentData(text string) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeText,
		Data: text,
	}, nil
}

// MarkdownContentData 创建 Markdown 类型的 ContentData
func MarkdownContentData(markdown string) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeMarkdown,
		Data: markdown,
	}, nil
}

// ThinkingContentData 创建 LLM 推理过程类型的 ContentData
func ThinkingContentData(thinking string) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeThinking,
		Data: thinking,
	}, nil
}

type SummaryItem struct {
	Role    string
	Content string
}

// SummaryContentData 创建 上下文摘要 类型的 ContentData
func SummaryContentData(summaryList []SummaryItem) (protocol.ContentData, error) {
	return protocol.ContentData{
		Type: protocol.ContentTypeSummary,
		Data: summaryList,
	}, nil
}

// SerializeContentData 将 ContentData 序列化为 JSON 字符串（用于存储到数据库 Content 字段）
func SerializeContentData(cd protocol.ContentData) (string, error) {
	jsonBytes, err := json.Marshal(cd)
	if err != nil {
		return "", fmt.Errorf("serialize content data: %w", err)
	}
	return string(jsonBytes), nil
}

// ParseContentData 从 JSON 字符串解析 ContentData
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

func ParseContentDataToolCall(data any) (protocol.ToolCall, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return protocol.ToolCall{}, fmt.Errorf("marshal tool call data: %w", err)
	}
	v := &protocol.ToolCall{}
	e := json.Unmarshal(bytes, v)
	return *v, e
}

// ContentDataBytes 将 ContentData.Data（any）重新序列化为 JSON 字节。
func ContentDataBytes(data any) ([]byte, error) {
	return json.Marshal(data)
}

// ContentDataString 将 ContentData.Data（any）提取为字符串。
// JSON 字符串 → 直接返回；其他类型 → 序列化为 JSON 字符串。
func ContentDataString(data any) (string, error) {
	if s, ok := data.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	// 如果序列化结果是 JSON 字符串（带引号），反序列化得到原始字符串
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return s, nil
	}
	return string(b), nil
}
