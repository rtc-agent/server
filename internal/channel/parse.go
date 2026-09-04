// Package channel 提供频道名的构造与解析工具。
//
// 频道格式约定：
//
//	Topic 频道（持久化可恢复）：topic:u={userID}
//	Live  频道（即发即弃瞬态）：live:u={userID}
//
// 本包为 handler、realtime 等包的频道解析提供统一实现，
// 消除各包中重复的字符串处理逻辑。
package channel

import "strings"

// ============================================================================
// 频道格式常量
// ============================================================================

const (
	// TopicPrefix Topic 频道前缀
	TopicPrefix = "topic:"
	// LivePrefix Live 频道前缀
	LivePrefix = "live:"
)

// ============================================================================
// 频道构造函数
// ============================================================================

// UserTopic 构造用户 Topic 频道：topic:u={userID}
func UserTopic(uid string) string {
	return "topic:u=" + uid
}

// UserLive 构造用户 Live 频道：live:u={userID}
func UserLive(uid string) string {
	return "live:u=" + uid
}

// ToLive 将 Topic 频道名转换为 Live 频道名
//
//	"topic:u=abc" → "live:u=abc"
func ToLive(topicCh string) string {
	return LivePrefix + strings.TrimPrefix(topicCh, TopicPrefix)
}

// ============================================================================
// 频道类型判断函数
// ============================================================================

// IsLive 判断频道是否为有效的 Live 频道（需包含用户标识）
func IsLive(ch string) bool {
	return strings.HasPrefix(ch, LivePrefix+"u=")
}

// IsTopic 判断频道是否为 Topic 频道（持久化可恢复）
func IsTopic(ch string) bool {
	return strings.HasPrefix(ch, TopicPrefix+"u=")
}

// IsUser 判断频道是否为用户频道（topic:u={userID} 或 live:u={userID}）
func IsUser(ch string) bool {
	return strings.HasPrefix(ch, TopicPrefix+"u=") || strings.HasPrefix(ch, LivePrefix+"u=")
}

// ============================================================================
// 频道解析函数
// ============================================================================

// ParseUser 从频道名中提取 userID。
//
// 支持格式（均兼容 topic/live 前缀）：
//
//	topic:u={userID}
//	live:u={userID}
//
// 返回 ok=false 表示不是用户频道。
func ParseUser(ch string) (userID string, ok bool) {
	var after string
	switch {
	case strings.HasPrefix(ch, TopicPrefix+"u="):
		after = strings.TrimPrefix(ch, TopicPrefix+"u=")
	case strings.HasPrefix(ch, LivePrefix+"u="):
		after = strings.TrimPrefix(ch, LivePrefix+"u=")
	default:
		return "", false
	}
	if after == "" {
		return "", false
	}
	return after, true
}
