package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateToTokens(t *testing.T) {
	const truncationMarker = "\n\n[Content truncated - exceeded token limit]\n\n"

	t.Run("short string not truncated", func(t *testing.T) {
		s := "hello world"
		// maxTokens=100 → maxChars=100*4/1.33=300. "hello world" is 11 bytes.
		result := truncateToTokens(s, 100)
		if result != s {
			t.Errorf("expected %q, got %q", s, result)
		}
	})

	t.Run("long string truncated with marker", func(t *testing.T) {
		// Create a string longer than maxChars.
		// maxTokens=10 → maxChars=10*4/1.33≈30
		s := strings.Repeat("a", 100)
		result := truncateToTokens(s, 10)
		if !strings.Contains(result, truncationMarker) {
			t.Error("expected truncation marker in result")
		}
		// Result should be shorter than original.
		if len(result) >= len(s) {
			t.Errorf("expected truncated result to be shorter than %d, got %d", len(s), len(result))
		}
	})

	t.Run("preserves UTF-8 boundaries for CJK characters", func(t *testing.T) {
		// CJK characters are 3 bytes each in UTF-8.
		// Create a string of CJK characters.
		cjk := strings.Repeat("中文测试", 20) // 80 bytes (20 * 4 chars * ~3 bytes each, actually 4*3=12 per "中文测试")
		// Actually "中文测试" = 4 runes * 3 bytes = 12 bytes, 20 times = 240 bytes
		maxTokens := 10 // maxChars = 10 * 4 / 1.33 ≈ 30
		result := truncateToTokens(cjk, maxTokens)

		// Verify the result is valid UTF-8.
		if !utf8.ValidString(result) {
			t.Error("truncated result is not valid UTF-8")
		}

		// Verify it contains the truncation marker.
		if !strings.Contains(result, truncationMarker) {
			t.Error("expected truncation marker in result")
		}
	})

	t.Run("preserves UTF-8 boundaries for emoji", func(t *testing.T) {
		// Emoji are 4 bytes each in UTF-8.
		emoji := strings.Repeat("😀", 50) // 200 bytes
		maxTokens := 10                     // maxChars ≈ 30
		result := truncateToTokens(emoji, maxTokens)

		if !utf8.ValidString(result) {
			t.Error("truncated result is not valid UTF-8")
		}
	})

	t.Run("mixed ASCII and multi-byte", func(t *testing.T) {
		// Mix of ASCII and CJK: "hello 你好 world 世界 ..."
		s := strings.Repeat("hello 你好 world 世界 ", 10) // ~230 bytes
		maxTokens := 10                                     // maxChars ≈ 30
		result := truncateToTokens(s, maxTokens)

		if !utf8.ValidString(result) {
			t.Error("truncated result is not valid UTF-8")
		}
		if !strings.Contains(result, truncationMarker) {
			t.Error("expected truncation marker in result")
		}
	})

	t.Run("head and tail content preserved", func(t *testing.T) {
		// Use distinct content for head and tail to verify both are kept.
		head := strings.Repeat("A", 50)
		tail := strings.Repeat("Z", 50)
		s := head + strings.Repeat("x", 200) + tail
		maxTokens := 20 // maxChars = 20 * 4 / 1.33 ≈ 60

		result := truncateToTokens(s, maxTokens)

		// Head should start with 'A's.
		if !strings.HasPrefix(result, "A") {
			t.Error("expected result to start with head content")
		}
		// Tail should end with 'Z's.
		if !strings.HasSuffix(result, "Z") {
			t.Error("expected result to end with tail content")
		}
	})
}

func TestTruncateToTokens_UTF8BoundaryPrecision(t *testing.T) {
	// Regression test: verify that truncation never splits a multi-byte character.
	// This was a bug where s[:headChars] could land in the middle of a UTF-8 sequence.
	testCases := []struct {
		name string
		s    string
	}{
		{"CJK at boundary", strings.Repeat("你好世界", 30)},
		{"emoji at boundary", strings.Repeat("🎉🎊🎈", 30)},
		{"mixed lengths", "a" + strings.Repeat("日本語", 50) + "z"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for maxTokens := 5; maxTokens < 50; maxTokens += 5 {
				result := truncateToTokens(tc.s, maxTokens)
				if !utf8.ValidString(result) {
					t.Errorf("maxTokens=%d produced invalid UTF-8", maxTokens)
				}
			}
		})
	}
}
