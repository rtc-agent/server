package cache_analyzer

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// CacheStats represents cache statistics from LLM response.
type CacheStats struct {
	InputTokens         int `json:"input_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CachedTokens        int `json:"cached_tokens"`
}

// TotalInputTokens calculates total input tokens (cached + non-cached).
func (c *CacheStats) TotalInputTokens() int {
	return c.InputTokens + c.CacheReadTokens + c.CacheCreationTokens
}

// HitRate calculates cache hit rate (cached / total).
func (c *CacheStats) HitRate() float64 {
	total := c.TotalInputTokens()
	if total == 0 {
		return 0.0
	}
	return float64(c.CacheReadTokens) / float64(total)
}

// HitRatePercent formats hit rate as percentage string.
func (c *CacheStats) HitRatePercent() string {
	return formatPercent(c.HitRate())
}

// Message represents a single message in the conversation.
type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"content"`       // 保持向后兼容（text 内容）
	ContentParts []ContentPart `json:"content_parts"` // 完整的 content 数组
	CacheControl string        `json:"cache_control,omitempty"`
	ContentHash  string        `json:"content_hash"`
}

// ContentPart represents a single part of message content.
type ContentPart struct {
	Type       string                 `json:"type"`               // "text", "tool_use", "tool_result"
	Text       string                 `json:"text,omitempty"`     // for text type
	ToolID     string                 `json:"id,omitempty"`       // for tool_use/tool_result
	ToolName   string                 `json:"name,omitempty"`     // for tool_use
	ToolInput  map[string]interface{} `json:"input,omitempty"`    // for tool_use
	ToolResult []ContentPart          `json:"content,omitempty"`  // for tool_result (nested content)
	IsError    bool                   `json:"is_error,omitempty"` // for tool_result
}

// SystemBlock represents a single block in the system prompt.
type SystemBlock struct {
	Text         string `json:"text"`
	CacheControl string `json:"cache_control,omitempty"`
	ContentHash  string `json:"content_hash"`
}

// LLMRequest represents a single LLM request with its response.
type LLMRequest struct {
	Timestamp    time.Time      `json:"timestamp"`
	SessionID    string         `json:"session_id"`
	Messages     []*Message     `json:"messages"`
	System       []*SystemBlock `json:"system"`
	CacheStats   *CacheStats    `json:"cache_stats,omitempty"`
	RequestIndex int            `json:"request_index"`
}

// MessageCount returns the number of messages.
func (r *LLMRequest) MessageCount() int {
	return len(r.Messages)
}

// ParseLogFile parses a log file and extracts requests and responses.
func ParseLogFile(logFile string) ([]*LLMRequest, []*LLMRequest, error) {
	file, err := os.Open(logFile)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	var requests []*LLMRequest
	var responses []*LLMRequest

	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		event, _ := entry["msg"].(string)
		sessionID, _ := entry["session_id"].(string)
		timeStr, _ := entry["time"].(string)

		timestamp, _ := time.Parse(time.RFC3339Nano, timeStr)

		switch event {
		case "llm.http.request":
			req, err := parseRequest(entry, timestamp, sessionID)
			if err == nil {
				req.RequestIndex = len(requests)
				requests = append(requests, req)
			}

		case "llm.http.response", "llm.http.response (stream complete)":
			resp := parseResponse(entry, timestamp, sessionID)
			if resp != nil {
				resp.RequestIndex = len(responses)
				responses = append(responses, resp)
			}
		}
	}

	return requests, responses, scanner.Err()
}

func parseRequest(entry map[string]interface{}, timestamp time.Time, sessionID string) (*LLMRequest, error) {
	requestBodyStr, _ := entry["request_body"].(string)
	if requestBodyStr == "" {
		return nil, nil
	}

	var requestBody map[string]interface{}
	if err := json.Unmarshal([]byte(requestBodyStr), &requestBody); err != nil {
		return nil, err
	}

	messagesRaw, _ := requestBody["messages"].([]interface{})
	var messages []*Message

	for _, msgRaw := range messagesRaw {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		contentParts, textContent := extractContentParts(msg["content"])
		cacheControl := extractCacheControl(msg["content"])

		messages = append(messages, &Message{
			Role:         role,
			Content:      textContent,
			ContentParts: contentParts,
			CacheControl: cacheControl,
			ContentHash:  computeHashFromParts(contentParts),
		})
	}

	// Parse system field
	systemRaw, _ := requestBody["system"].([]interface{})
	var system []*SystemBlock
	for _, sysRaw := range systemRaw {
		sysMap, ok := sysRaw.(map[string]interface{})
		if !ok {
			continue
		}
		text, _ := sysMap["text"].(string)
		cacheControl := ""
		if cc, exists := sysMap["cache_control"]; exists {
			if ccMap, ok := cc.(map[string]interface{}); ok {
				ccType, _ := ccMap["type"].(string)
				ccTTL, _ := ccMap["ttl"].(string)
				if ccTTL != "" {
					cacheControl = ccType + ":" + ccTTL
				} else {
					cacheControl = ccType
				}
			}
		}
		system = append(system, &SystemBlock{
			Text:         text,
			CacheControl: cacheControl,
			ContentHash:  computeHash(text),
		})
	}

	return &LLMRequest{
		Timestamp: timestamp,
		SessionID: sessionID,
		Messages:  messages,
		System:    system,
	}, nil
}

func parseResponse(entry map[string]interface{}, timestamp time.Time, sessionID string) *LLMRequest {
	responseBodyStr, _ := entry["response_body"].(string)
	if responseBodyStr == "" {
		return nil
	}

	cacheStats := extractCacheStats(responseBodyStr)

	return &LLMRequest{
		Timestamp:  timestamp,
		SessionID:  sessionID,
		CacheStats: cacheStats,
	}
}

// extractContentParts extracts all content parts from message content.
// Returns both structured parts and concatenated text for backward compatibility.
func extractContentParts(content interface{}) ([]ContentPart, string) {
	var parts []ContentPart
	var textContent string

	switch v := content.(type) {
	case string:
		textContent = v
		parts = append(parts, ContentPart{Type: "text", Text: v})
	case []interface{}:
		for _, part := range v {
			if partMap, ok := part.(map[string]interface{}); ok {
				partType, _ := partMap["type"].(string)
				switch partType {
				case "text":
					if text, _ := partMap["text"].(string); text != "" {
						parts = append(parts, ContentPart{Type: "text", Text: text})
						textContent += text
					}
				case "tool_use":
					cp := ContentPart{Type: "tool_use"}
					if id, ok := partMap["id"].(string); ok {
						cp.ToolID = id
					}
					if name, ok := partMap["name"].(string); ok {
						cp.ToolName = name
					}
					if input, ok := partMap["input"].(map[string]interface{}); ok {
						cp.ToolInput = input
					}
					parts = append(parts, cp)
				case "tool_result":
					cp := ContentPart{Type: "tool_result"}
					if id, ok := partMap["tool_use_id"].(string); ok {
						cp.ToolID = id
					}
					if isError, ok := partMap["is_error"].(bool); ok {
						cp.IsError = isError
					}
					// Recursively extract nested content
					if nestedContent, ok := partMap["content"]; ok {
						nestedParts, nestedText := extractContentParts(nestedContent)
						cp.ToolResult = nestedParts
						textContent += nestedText
					}
					parts = append(parts, cp)
				}
			}
		}
	}
	return parts, textContent
}

func extractCacheControl(content interface{}) string {
	if parts, ok := content.([]interface{}); ok {
		for _, part := range parts {
			if partMap, ok := part.(map[string]interface{}); ok {
				if cc, exists := partMap["cache_control"]; exists {
					if ccMap, ok := cc.(map[string]interface{}); ok {
						ccType, _ := ccMap["type"].(string)
						ccTTL, _ := ccMap["ttl"].(string)
						if ccTTL != "" {
							return ccType + ":" + ccTTL
						}
						return ccType
					}
				}
			}
		}
	}
	return ""
}

func extractCacheStats(responseBody string) *CacheStats {
	// Check if it's SSE format
	if strings.Contains(responseBody, "event:") && strings.Contains(responseBody, "data:") {
		return extractCacheStatsFromSSE(responseBody)
	}

	// Try JSON format
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(responseBody), &body); err != nil {
		return nil
	}

	if usage, ok := body["usage"].(map[string]interface{}); ok {
		return parseUsage(usage)
	}

	return nil
}

// parseMessageDeltaUsage extracts cache stats from message_delta event usage.
func parseMessageDeltaUsage(usage map[string]interface{}) (outputTokens, cacheCreationTokens, cacheReadTokens, cachedTokens int) {
	if v, ok := usage["output_tokens"].(float64); ok {
		outputTokens = int(v)
	}
	if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
		cacheCreationTokens = int(v)
	}
	if v, ok := usage["cache_read_input_tokens"].(float64); ok {
		cacheReadTokens = int(v)
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
		if v, ok := details["cached_tokens"].(float64); ok {
			cachedTokens = int(v)
		}
	}
	// Check nested cache_creation object
	if cc, ok := usage["cache_creation"].(map[string]interface{}); ok {
		if cacheCreationTokens == 0 {
			if v, ok := cc["ephemeral_5m_input_tokens"].(float64); ok {
				cacheCreationTokens = int(v)
			}
		}
	}
	return
}

func extractCacheStatsFromSSE(responseBody string) *CacheStats {
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, cachedTokens int

	for _, line := range strings.Split(responseBody, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		jsonStr := strings.TrimSpace(line[5:])
		if jsonStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			continue
		}

		eventType, _ := data["type"].(string)

		switch eventType {
		case "message_start":
			if message, ok := data["message"].(map[string]interface{}); ok {
				if usage, ok := message["usage"].(map[string]interface{}); ok {
					if v, ok := usage["input_tokens"].(float64); ok {
						inputTokens = int(v)
					}
				}
			}

		case "message_delta":
			if usage, ok := data["usage"].(map[string]interface{}); ok {
				outputTokens, cacheCreationTokens, cacheReadTokens, cachedTokens = parseMessageDeltaUsage(usage)
			}
		}
	}

	if inputTokens > 0 || outputTokens > 0 {
		return &CacheStats{
			InputTokens:         inputTokens,
			CacheCreationTokens: cacheCreationTokens,
			CacheReadTokens:     cacheReadTokens,
			CachedTokens:        cachedTokens,
		}
	}

	return nil
}

func parseUsage(usage map[string]interface{}) *CacheStats {
	var stats CacheStats

	if v, ok := usage["input_tokens"].(float64); ok {
		stats.InputTokens = int(v)
	}
	if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
		stats.CacheCreationTokens = int(v)
	}
	if v, ok := usage["cache_read_input_tokens"].(float64); ok {
		stats.CacheReadTokens = int(v)
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
		if v, ok := details["cached_tokens"].(float64); ok {
			stats.CachedTokens = int(v)
		}
	}

	return &stats
}

// MergeAndFilterRequests merges requests and responses from multiple log files,
// filters by session ID, and sorts by timestamp.
func MergeAndFilterRequests(logFiles []string, sessionID string) ([]*LLMRequest, error) {
	var allRequests []*LLMRequest
	var allResponses []*LLMRequest

	for _, logFile := range logFiles {
		requests, responses, err := ParseLogFile(logFile)
		if err != nil {
			continue
		}

		// Filter by session ID
		for _, req := range requests {
			if req.SessionID == sessionID {
				allRequests = append(allRequests, req)
			}
		}
		for _, resp := range responses {
			if resp.SessionID == sessionID {
				allResponses = append(allResponses, resp)
			}
		}
	}

	// Sort by timestamp
	sort.Slice(allRequests, func(i, j int) bool {
		return allRequests[i].Timestamp.Before(allRequests[j].Timestamp)
	})
	sort.Slice(allResponses, func(i, j int) bool {
		return allResponses[i].Timestamp.Before(allResponses[j].Timestamp)
	})

	// Match requests with responses
	for i, req := range allRequests {
		if i < len(allResponses) {
			req.CacheStats = allResponses[i].CacheStats
		}
		// Update request index
		req.RequestIndex = i
	}

	return allRequests, nil
}

// computeHash computes MD5 hash of a string.
func computeHash(text string) string {
	if text == "" {
		return "00000000"
	}
	hash := md5.Sum([]byte(text))
	return hex.EncodeToString(hash[:8])[:8]
}

// computeHashFromParts computes hash from complete content parts.
func computeHashFromParts(parts []ContentPart) string {
	if len(parts) == 0 {
		return "00000000"
	}

	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(part.Type)
		builder.WriteString("|")

		switch part.Type {
		case "text":
			builder.WriteString(part.Text)
		case "tool_use":
			builder.WriteString(part.ToolID)
			builder.WriteString("|")
			builder.WriteString(part.ToolName)
			builder.WriteString("|")
			if inputJSON, err := json.Marshal(part.ToolInput); err == nil {
				builder.Write(inputJSON)
			}
		case "tool_result":
			builder.WriteString(part.ToolID)
			builder.WriteString("|")
			if part.IsError {
				builder.WriteString("error")
			} else {
				builder.WriteString("ok")
			}
			builder.WriteString("|")
			// Recursively hash nested content
			builder.WriteString(computeHashFromParts(part.ToolResult))
		}
		builder.WriteString("|")
	}

	hash := md5.Sum([]byte(builder.String()))
	return hex.EncodeToString(hash[:8])[:8]
}
