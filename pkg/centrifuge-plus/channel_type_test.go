package centrifugeplus

import "testing"

func TestChannelType_String(t *testing.T) {
	tests := []struct {
		ct       ChannelType
		expected string
	}{
		{Live, "live"},
		{Topic, "topic"},
		{ChannelType(99), "live"}, // 未知类型默认返回 "live"
		{ChannelType(-1), "live"},
	}

	for _, tt := range tests {
		if got := tt.ct.String(); got != tt.expected {
			t.Errorf("ChannelType(%d).String() = %q, want %q", tt.ct, got, tt.expected)
		}
	}
}
