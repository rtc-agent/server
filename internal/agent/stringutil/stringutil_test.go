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
	// "你好" is 6 bytes in UTF-8 (3 bytes each)
	s := "你好"
	got := TruncateByByte(s, 4)
	// Should truncate to 4 bytes + "...", which splits the second character
	want := s[:4] + "..."
	if got != want {
		t.Errorf("TruncateByByte(%q, 4) = %q, want %q", s, got, want)
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
