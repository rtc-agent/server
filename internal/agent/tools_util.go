package agent

import (
	"encoding/json"

	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
)

// extractTextFromContent extracts human-readable text from a ContentData.
// For text/markdown types, returns the string data directly.
// For user_message type, extracts the text field from the structured content.
// For other types, falls back to JSON-stringifying the data.
func extractTextFromContent(content protocol.ContentData) string {
	switch content.Type {
	case protocol.ContentTypeText, protocol.ContentTypeMarkdown:
		if text, ok := content.Data.(string); ok {
			return text
		}
	case protocol.ContentTypeUserMessage:
		if umc, err := primitives.ParseUserMessageContent(content.Data); err == nil {
			return umc.Text
		}
	}
	dataBytes, _ := json.Marshal(content.Data)
	return string(dataBytes)
}
