// Package stringutil provides shared string manipulation helpers.
//
// Multiple packages within the agent subsystem had duplicated truncation
// logic with only minor differences (byte-level vs rune-level). This package
// consolidates them into a single, well-tested location.
package stringutil

import "unicode/utf8"

// TruncateByByte truncates s to at most maxLen bytes. If s is longer than
// maxLen, the result is s[:cut] + "..." where cut is adjusted to avoid
// splitting a multi-byte UTF-8 character.
//
// Strategy:
//  1. If s[maxLen] starts a new character, cut at maxLen.
//  2. Otherwise, walk backward to find the start of the character spanning maxLen.
//  3. If that character fits entirely within maxLen (lead + all bytes), include it.
//  4. Otherwise, cut before the character starts.
//  5. Fallback: if no character start is found before position 0, cut at maxLen.
func TruncateByByte(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}

	// Fast path: if the byte at maxLen starts a character, we can cut exactly.
	if utf8.RuneStart(s[maxLen]) {
		return s[:maxLen] + "..."
	}

	// The byte at maxLen is a continuation byte. Walk backward to find the
	// lead byte of the multi-byte character that spans across maxLen.
	charStart := maxLen
	for charStart > 0 && !utf8.RuneStart(s[charStart]) {
		charStart--
	}

	if charStart == 0 && !utf8.RuneStart(s[0]) {
		// Fallback: no character start found (malformed UTF-8 at beginning).
		// Cut at maxLen — best effort for invalid input.
		return s[:maxLen] + "..."
	}

	// charStart is the lead byte of the multi-byte character spanning maxLen.
	// Check if the full character fits within maxLen.
	leadByte := s[charStart]
	var charLen int
	switch {
	case leadByte < 0x80:
		charLen = 1
	case leadByte < 0xE0:
		charLen = 2
	case leadByte < 0xF0:
		charLen = 3
	default:
		charLen = 4
	}

	if charStart+charLen <= maxLen {
		// Full character fits within maxLen — include it.
		return s[:charStart+charLen] + "..."
	}
	// Character extends past maxLen — cut before it starts.
	return s[:charStart] + "..."
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
