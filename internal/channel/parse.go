// Package channel provides utilities for constructing and parsing Centrifuge
// channel names.
//
// Channel naming convention:
//
//	Topic channel (persistent, recoverable): topic:u={userID}
//	Live  channel (ephemeral, fire-and-forget): live:u={userID}
//
// This package centralises channel parsing so that handler, realtime, and
// other packages share a single implementation instead of duplicating
// string-processing logic.
package channel

import "strings"

// ============================================================================
// Channel format constants
// ============================================================================

const (
	// TopicPrefix is the prefix for Topic channels (persistent, recoverable).
	TopicPrefix = "topic:"
	// LivePrefix is the prefix for Live channels (ephemeral, fire-and-forget).
	LivePrefix = "live:"
)

// ============================================================================
// Channel constructors
// ============================================================================

// UserTopic constructs a user Topic channel: topic:u={userID}
func UserTopic(uid string) string {
	return "topic:u=" + uid
}

// UserLive constructs a user Live channel: live:u={userID}
func UserLive(uid string) string {
	return "live:u=" + uid
}

// ToLive converts a Topic channel name to its Live counterpart.
// Returns empty string if the input is not a valid Topic channel.
//
//	"topic:u=abc" -> "live:u=abc"
func ToLive(topicCh string) string {
	if !IsTopic(topicCh) {
		return "" // 非 Topic 频道无法转换
	}
	return LivePrefix + strings.TrimPrefix(topicCh, TopicPrefix)
}

// ============================================================================
// Channel type predicates
// ============================================================================

// IsLive reports whether ch is a valid Live channel (must include user identifier).
func IsLive(ch string) bool {
	return strings.HasPrefix(ch, LivePrefix+"u=")
}

// IsTopic reports whether ch is a Topic channel (persistent, recoverable).
func IsTopic(ch string) bool {
	return strings.HasPrefix(ch, TopicPrefix+"u=")
}

// IsUser reports whether ch is a user channel (topic:u={userID} or live:u={userID}).
//
// Note: IsUser and ParseUser share the same channel format logic. IsUser is a fast
// predicate for channel type checking, while ParseUser extracts the userID. They are
// kept separate for clarity and performance — IsUser avoids the string allocations
// that ParseUser performs. If you need both checks, call ParseUser and check ok=true.
func IsUser(ch string) bool {
	return strings.HasPrefix(ch, TopicPrefix+"u=") || strings.HasPrefix(ch, LivePrefix+"u=")
}

// ============================================================================
// Channel parsing
// ============================================================================

// ParseUser extracts the userID from a channel name.
//
// Supported formats (both accept topic/live prefix):
//
//	topic:u={userID}
//	live:u={userID}
//
// Returns ok=false when the channel is not a user channel.
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
	// Reject userID containing separators to prevent channel name injection
	if strings.ContainsAny(after, ":=") {
		return "", false
	}
	return after, true
}
