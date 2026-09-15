// Package stringutil provides shared string manipulation helpers.
//
// Multiple packages within the agent subsystem had duplicated truncation
// logic with only minor differences (byte-level vs rune-level). This package
// consolidates them into a single, well-tested location.
package stringutil

import "unicode/utf8"

// TruncateByByte truncates s to at most maxLen bytes. If s is longer than
// maxLen, the result is s[:cut] + "..." where cut is adjusted to avoid
// splitting a multi-byte UTF-8 character. If maxLen falls in the middle of
// a multi-byte character, cut is backed up to its start; if no character
// start is found before maxLen, the raw maxLen cut is used as a fallback.
func TruncateByByte(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Back up to the nearest valid UTF-8 character boundary.
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		cut = maxLen
	}
	return s[:cut] + "..."
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
