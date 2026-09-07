package agent

import (
	"encoding/json"

	"github.com/rtc-agent/server/pkg/protocol"
)

// extractTextFromContent extracts human-readable text from a ContentData.
// For text/markdown types, returns the string data directly.
// For other types, falls back to JSON-stringifying the data.
func extractTextFromContent(content protocol.ContentData) string {
	if content.Type == protocol.ContentTypeText || content.Type == protocol.ContentTypeMarkdown {
		if text, ok := content.Data.(string); ok {
			return text
		}
	}
	dataBytes, _ := json.Marshal(content.Data)
	return string(dataBytes)
}
