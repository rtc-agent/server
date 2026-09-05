package channel

import "testing"

func TestUserTopic(t *testing.T) {
	tests := []struct {
		uid      string
		expected string
	}{
		{"123", "topic:u=123"},
		{"user_abc", "topic:u=user_abc"},
	}

	for _, tt := range tests {
		result := UserTopic(tt.uid)
		if result != tt.expected {
			t.Errorf("UserTopic(%q) = %q, want %q", tt.uid, result, tt.expected)
		}
	}
}

func TestUserLive(t *testing.T) {
	tests := []struct {
		uid      string
		expected string
	}{
		{"123", "live:u=123"},
		{"user_abc", "live:u=user_abc"},
	}

	for _, tt := range tests {
		result := UserLive(tt.uid)
		if result != tt.expected {
			t.Errorf("UserLive(%q) = %q, want %q", tt.uid, result, tt.expected)
		}
	}
}

func TestToLive(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"topic:u=123", "live:u=123"},
		{"topic:u=abc", "live:u=abc"},
	}

	for _, tt := range tests {
		result := ToLive(tt.input)
		if result != tt.expected {
			t.Errorf("ToLive(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestIsTopic(t *testing.T) {
	tests := []struct {
		ch       string
		expected bool
	}{
		{"topic:u=123", true},
		{"topic:u=abc", true},
		{"live:u=123", false},
		{"invalid", false},
		{"topic:", false},
	}

	for _, tt := range tests {
		result := IsTopic(tt.ch)
		if result != tt.expected {
			t.Errorf("IsTopic(%q) = %v, want %v", tt.ch, result, tt.expected)
		}
	}
}

func TestIsLive(t *testing.T) {
	tests := []struct {
		ch       string
		expected bool
	}{
		{"live:u=123", true},
		{"live:u=abc", true},
		{"topic:u=123", false},
		{"invalid", false},
		{"live:", false},
	}

	for _, tt := range tests {
		result := IsLive(tt.ch)
		if result != tt.expected {
			t.Errorf("IsLive(%q) = %v, want %v", tt.ch, result, tt.expected)
		}
	}
}

func TestIsUser(t *testing.T) {
	tests := []struct {
		ch       string
		expected bool
	}{
		{"topic:u=123", true},
		{"live:u=123", true},
		{"topic:system", false},
		{"invalid", false},
	}

	for _, tt := range tests {
		result := IsUser(tt.ch)
		if result != tt.expected {
			t.Errorf("IsUser(%q) = %v, want %v", tt.ch, result, tt.expected)
		}
	}
}

func TestParseUser(t *testing.T) {
	tests := []struct {
		ch          string
		expectedUID string
		expectedOK  bool
	}{
		{"topic:u=123", "123", true},
		{"live:u=456", "456", true},
		{"topic:u=abc", "abc", true},
		{"topic:", "", false},
		{"invalid", "", false},
		{"topic:system", "", false},
	}

	for _, tt := range tests {
		uid, ok := ParseUser(tt.ch)
		if uid != tt.expectedUID || ok != tt.expectedOK {
			t.Errorf("ParseUser(%q) = (%q, %v), want (%q, %v)", tt.ch, uid, ok, tt.expectedUID, tt.expectedOK)
		}
	}
}
