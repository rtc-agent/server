// Package stringutil provides shared string manipulation helpers.
//
// Multiple packages within the agent subsystem had duplicated truncation
// logic with only minor differences (byte-level vs rune-level). This package
// consolidates them into a single, well-tested location.
package stringutil

// TruncateByByte truncates s to at most maxLen bytes. If s is longer than
// maxLen, the result is s[:maxLen] + "...".
//
// WARNING: This may split multi-byte UTF-8 characters. Use TruncateByRune
// when the output will be displayed to users.
func TruncateByByte(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// TruncateByRune truncates s to at most maxLen runes. If s has more than
// maxLen runes, the result is the first maxLen runes + "...".
//
// This is safe for multi-byte UTF-8 characters.
func TruncateByRune(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
