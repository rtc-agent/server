// internal/usecase/primitives/util.go
package primitives

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// TruncateTitle 截取 content 第一行的前 maxRunes 个 rune 作为标题。
// 行为与旧 usecase/message.go 的 truncateTitle 完全一致。
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

// ValidateCreateMessageRequest 校验 SendMessage 请求。
func ValidateCreateMessageRequest(content string) error {
	if content == "" {
		return fmt.Errorf("content must not be empty")
	}
	return nil
}
