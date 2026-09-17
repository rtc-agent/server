package agent

import (
	"encoding/json"
	"fmt"

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
	dataBytes, err := json.Marshal(content.Data)
	if err != nil {
		// Fall back to fmt.Sprintf on JSON serialization failure to avoid losing debug info.
		return fmt.Sprintf("%v", content.Data)
	}
	return string(dataBytes)
}
