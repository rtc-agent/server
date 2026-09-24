package agent

import (
	"testing"

	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestSetCacheBreakpoints(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []*turnagent.Message
		wantBPs int
		checkFn func(t *testing.T, msgs []*turnagent.Message)
	}{
		{
			name: "normal case with summary boundary",
			msgs: []*turnagent.Message{
				{Role: "system", Content: "system prompt"},
				{Role: "system", Content: "attachments"},
				{Role: "system", Content: "scenarios"},
				{Role: "system", Content: "command prompts"},
				{Role: "system", Content: "summary content", Extra: map[string]any{turnagent.ExtraKeySummaryBoundary: true}},
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
			},
			wantBPs: 1, // BUG 14 fix: only bp1 (summary), bp2 removed
			checkFn: func(t *testing.T, msgs []*turnagent.Message) {
				// bp1: summary boundary (index 4)
				if !msgs[4].CacheBreakpoint {
					t.Error("expected bp on summary boundary message")
				}
				if msgs[4].CacheTTL != "1h" {
					t.Errorf("expected TTL 1h on summary, got %s", msgs[4].CacheTTL)
				}
				// bp2: REMOVED (BUG 14 fix) - last conversation message should NOT have breakpoint
				if msgs[6].CacheBreakpoint {
					t.Error("bp2 removed: last conversation message should NOT have breakpoint")
				}
			},
		},
		{
			name: "no summary message",
			msgs: []*turnagent.Message{
				{Role: "system", Content: "system prompt"},
				{Role: "user", Content: "hello"},
			},
			wantBPs: 0, // BUG 14 fix: no summary means no breakpoints
			checkFn: func(t *testing.T, msgs []*turnagent.Message) {
				// No summary boundary, so no breakpoints
				for i, m := range msgs {
					if m.CacheBreakpoint {
						t.Errorf("no breakpoints expected, but message %d has one", i)
					}
				}
			},
		},
		{
			name: "less than 3 messages",
			msgs: []*turnagent.Message{
				{Role: "user", Content: "hello"},
			},
			wantBPs: 0, // BUG 14 fix: no summary means no breakpoints
		},
		{
			name:    "empty messages",
			msgs:    []*turnagent.Message{},
			wantBPs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &helpers{enableStrategicCacheBreakpoints: true}
			result := h.setCacheBreakpoints(tt.msgs)
			bpCount := 0
			for _, m := range result {
				if m.CacheBreakpoint {
					bpCount++
				}
			}
			if bpCount != tt.wantBPs {
				t.Errorf("got %d breakpoints, want %d", bpCount, tt.wantBPs)
			}
			if tt.checkFn != nil {
				tt.checkFn(t, result)
			}
		})
	}
}

func TestIsSummaryMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  *turnagent.Message
		want bool
	}{
		{
			name: "with summary boundary marker",
			msg:  &turnagent.Message{Role: "system", Content: "summary", Extra: map[string]any{turnagent.ExtraKeySummaryBoundary: true}},
			want: true,
		},
		{
			name: "without marker",
			msg:  &turnagent.Message{Role: "system", Content: "summary"},
			want: false,
		},
		{
			name: "nil Extra",
			msg:  &turnagent.Message{Role: "system", Content: "summary", Extra: nil},
			want: false,
		},
		{
			name: "marker is not bool",
			msg:  &turnagent.Message{Role: "system", Content: "summary", Extra: map[string]any{turnagent.ExtraKeySummaryBoundary: "not_bool"}},
			want: false,
		},
		{
			name: "marker is false",
			msg:  &turnagent.Message{Role: "system", Content: "summary", Extra: map[string]any{turnagent.ExtraKeySummaryBoundary: false}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSummaryMessage(tt.msg); got != tt.want {
				t.Errorf("isSummaryMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}
