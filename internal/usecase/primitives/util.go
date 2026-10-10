// internal/usecase/primitives/util.go
package primitives

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// TruncateTitle extracts the first line of content and truncates to maxRunes runes
// for use as a title. Behavior is identical to the old truncateTitle in
// usecase/message.go.
func TruncateTitle(content string, maxRunes int) string {
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		content = content[:idx]
	}
	content = strings.TrimSpace(content)
	if utf8.RuneCountInString(content) <= maxRunes {
		return content
	}
	runes := []rune(content)
	return string(runes[:maxRunes])
}

// ValidateCreateMessageRequest validates a SendMessage request.
// Content must be non-empty and not exceed 100KB (102,400 bytes).
func ValidateCreateMessageRequest(content string) error {
	if content == "" {
		return fmt.Errorf("content must not be empty")
	}
	const maxContentLength = 100 * 1024 // 100KB
	if len(content) > maxContentLength {
		return fmt.Errorf("content too long: %d bytes (max %d)", len(content), maxContentLength)
	}
	return nil
}
