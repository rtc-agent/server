package stringutil

import "testing"

func TestTruncateByByte_ShorterThanLimit(t *testing.T) {
	got := TruncateByByte("hello", 10)
	if got != "hello" {
		t.Errorf("TruncateByByte(%q, 10) = %q, want %q", "hello", got, "hello")
	}
}

func TestTruncateByByte_ExactlyAtLimit(t *testing.T) {
	got := TruncateByByte("hello", 5)
	if got != "hello" {
		t.Errorf("TruncateByByte(%q, 5) = %q, want %q", "hello", got, "hello")
	}
}

func TestTruncateByByte_Truncated(t *testing.T) {
	got := TruncateByByte("hello world", 5)
	want := "hello..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 5) = %q, want %q", "hello world", got, want)
	}
}

func TestTruncateByByte_EmptyString(t *testing.T) {
	got := TruncateByByte("", 5)
	if got != "" {
		t.Errorf("TruncateByByte(%q, 5) = %q, want %q", "", got, "")
	}
}

func TestTruncateByByte_ZeroLimit(t *testing.T) {
	got := TruncateByByte("hello", 0)
	want := "..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 0) = %q, want %q", "hello", got, want)
	}
}

func TestTruncateByByte_MultiByteCharacters(t *testing.T) {
	// "你好" is 6 bytes in UTF-8 (3 bytes each).
	// Truncating at byte 4 (mid-character) should back up to byte 3 (start of
	// second character) to avoid splitting the multi-byte rune.
	s := "你好"
	got := TruncateByByte(s, 4)
	want := "你..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 4) = %q, want %q", s, got, want)
	}
}

func TestTruncateByByte_MultiByteExactBoundary(t *testing.T) {
	// Truncating at byte 3 (exact character boundary) should not lose a char.
	s := "你好世界"
	got := TruncateByByte(s, 3)
	want := "你..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 3) = %q, want %q", s, got, want)
	}
}

func TestTruncateByByte_FourByteCharMidChar(t *testing.T) {
	// "a😀b" is 6 bytes: 'a'(1) + '😀'(4) + 'b'(1).
	// Truncating at byte 3 (middle of 4-byte emoji) should cut before the emoji.
	s := "a😀b"
	got := TruncateByByte(s, 3)
	want := "a..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 3) = %q, want %q", s, got, want)
	}
}

func TestTruncateByByte_FourByteCharFits(t *testing.T) {
	// "a😀b" is 6 bytes: 'a'(1) + '😀'(4) + 'b'(1).
	// Truncating at byte 5 (end of 4-byte emoji) should include the full emoji.
	s := "a😀b"
	got := TruncateByByte(s, 5)
	want := "a😀..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 5) = %q, want %q", s, got, want)
	}
}

func TestTruncateByByte_TwoByteCharMidChar(t *testing.T) {
	// "aé" is 3 bytes: 'a'(1) + 'é'(2).
	// Truncating at byte 2 (middle of 2-byte char) should cut before 'é'.
	s := "aé"
	got := TruncateByByte(s, 2)
	want := "a..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 2) = %q, want %q", s, got, want)
	}
}

func TestTruncateByByte_AlwaysValidUTF8(t *testing.T) {
	// Verify the output is always valid UTF-8 for various inputs and maxLen values.
	testCases := []struct {
		s      string
		maxLen int
	}{
		{"你好世界", 1},
		{"你好世界", 2},
		{"你好世界", 4},
		{"你好世界", 5},
		{"😀😁😂", 1},
		{"😀😁😂", 3},
		{"😀😁😂", 5},
		{"abc😀def", 4},
		{"abc😀def", 5},
		{"abc😀def", 6},
	}
	for _, tc := range testCases {
		got := TruncateByByte(tc.s, tc.maxLen)
		// Ensure the result is valid UTF-8 (no unpaired surrogates).
		for i, r := range got {
			if r == '�' {
				// Check if it's a real replacement char in input vs. invalid byte.
				if i < len(tc.s) && tc.s[i] != 0xEF {
					t.Errorf("TruncateByByte(%q, %d) = %q, contains invalid UTF-8 at byte %d", tc.s, tc.maxLen, got, i)
				}
			}
		}
	}
}

func TestTruncateByRune_ShorterThanLimit(t *testing.T) {
	got := TruncateByRune("hello", 10)
	if got != "hello" {
		t.Errorf("TruncateByRune(%q, 10) = %q, want %q", "hello", got, "hello")
	}
}

func TestTruncateByRune_ExactlyAtLimit(t *testing.T) {
	got := TruncateByRune("hello", 5)
	if got != "hello" {
		t.Errorf("TruncateByRune(%q, 5) = %q, want %q", "hello", got, "hello")
	}
}

func TestTruncateByRune_Truncated(t *testing.T) {
	got := TruncateByRune("hello world", 5)
	want := "hello..."
	if got != want {
		t.Errorf("TruncateByRune(%q, 5) = %q, want %q", "hello world", got, want)
	}
}

func TestTruncateByRune_EmptyString(t *testing.T) {
	got := TruncateByRune("", 5)
	if got != "" {
		t.Errorf("TruncateByRune(%q, 5) = %q, want %q", "", got, "")
	}
}

func TestTruncateByRune_ZeroLimit(t *testing.T) {
	got := TruncateByRune("hello", 0)
	want := "..."
	if got != want {
		t.Errorf("TruncateByRune(%q, 0) = %q, want %q", "hello", got, want)
	}
}

func TestTruncateByRune_Chinese(t *testing.T) {
	got := TruncateByRune("你好世界", 2)
	want := "你好..."
	if got != want {
		t.Errorf("TruncateByRune(%q, 2) = %q, want %q", "你好世界", got, want)
	}
}

func TestTruncateByRune_Emoji(t *testing.T) {
	got := TruncateByRune("😀😁😂😃", 2)
	want := "😀😁..."
	if got != want {
		t.Errorf("TruncateByRune(%q, 2) = %q, want %q", "😀😁😂😃", got, want)
	}
}

func TestTruncateByRune_MixedMultiByte(t *testing.T) {
	// "a你好b" has 4 runes
	got := TruncateByRune("a你好b", 3)
	want := "a你好..."
	if got != want {
		t.Errorf("TruncateByRune(%q, 3) = %q, want %q", "a你好b", got, want)
	}
}
