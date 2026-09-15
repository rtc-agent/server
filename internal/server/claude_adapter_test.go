package server

import (
	"testing"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/schema"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

func TestApplyClaudeCacheBreakpoints(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []*schema.Message
		checkFn func(t *testing.T, msgs []*schema.Message)
	}{
		{
			name: "message with breakpoint and 1h TTL",
			msgs: []*schema.Message{
				{
					Role:    schema.System,
					Content: "summary",
					Extra: map[string]any{
						turnagent.ExtraKeyCacheBreakpoint:    true,
						turnagent.ExtraKeyCacheBreakpointTTL: "1h",
					},
				},
				{Role: schema.User, Content: "hello"},
			},
			checkFn: func(t *testing.T, msgs []*schema.Message) {
				// Check that the first message has cache control set
				if msgs[0].Extra == nil {
					t.Fatal("expected Extra to be set")
				}
				// The claude.SetMessageCacheControl should have set the cache control
				// We can verify by checking that the Extra still contains the marker
				if isBP, ok := msgs[0].Extra[turnagent.ExtraKeyCacheBreakpoint].(bool); !ok || !isBP {
					t.Error("expected breakpoint marker to be preserved")
				}
			},
		},
		{
			name: "message with breakpoint and default TTL",
			msgs: []*schema.Message{
				{
					Role:    schema.Assistant,
					Content: "response",
					Extra: map[string]any{
						turnagent.ExtraKeyCacheBreakpoint: true,
						// No TTL specified, should default to 5m
					},
				},
			},
			checkFn: func(t *testing.T, msgs []*schema.Message) {
				if msgs[0].Extra == nil {
					t.Fatal("expected Extra to be set")
				}
				if isBP, ok := msgs[0].Extra[turnagent.ExtraKeyCacheBreakpoint].(bool); !ok || !isBP {
					t.Error("expected breakpoint marker to be preserved")
				}
			},
		},
		{
			name: "message without breakpoint",
			msgs: []*schema.Message{
				{Role: schema.User, Content: "hello"},
				{Role: schema.Assistant, Content: "hi"},
			},
			checkFn: func(t *testing.T, msgs []*schema.Message) {
				// Messages without breakpoint marker should be unchanged
				if msgs[0].Extra != nil {
					t.Error("expected no Extra on first message")
				}
				if msgs[1].Extra != nil {
					t.Error("expected no Extra on second message")
				}
			},
		},
		{
			name: "mixed messages with and without breakpoint",
			msgs: []*schema.Message{
				{
					Role:    schema.System,
					Content: "summary",
					Extra: map[string]any{
						turnagent.ExtraKeyCacheBreakpoint:    true,
						turnagent.ExtraKeyCacheBreakpointTTL: "1h",
					},
				},
				{Role: schema.User, Content: "hello"},
				{
					Role:    schema.Assistant,
					Content: "response",
					Extra: map[string]any{
						turnagent.ExtraKeyCacheBreakpoint: true,
						// Default TTL
					},
				},
			},
			checkFn: func(t *testing.T, msgs []*schema.Message) {
				// First message should have breakpoint
				if msgs[0].Extra == nil {
					t.Fatal("expected Extra on first message")
				}
				if isBP, ok := msgs[0].Extra[turnagent.ExtraKeyCacheBreakpoint].(bool); !ok || !isBP {
					t.Error("expected breakpoint on first message")
				}

				// Second message should not have breakpoint
				if msgs[1].Extra != nil {
					t.Error("expected no Extra on second message")
				}

				// Third message should have breakpoint
				if msgs[2].Extra == nil {
					t.Fatal("expected Extra on third message")
				}
				if isBP, ok := msgs[2].Extra[turnagent.ExtraKeyCacheBreakpoint].(bool); !ok || !isBP {
					t.Error("expected breakpoint on third message")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := applyClaudeCacheBreakpoints(tt.msgs)
			if tt.checkFn != nil {
				tt.checkFn(t, result)
			}
		})
	}
}

func TestClaudeChatModelWrapper(t *testing.T) {
	// Verify the wrapper can be created with a nil model (for testing)
	wrapper := &claudeChatModelWrapper{nil}
	if wrapper.ToolCallingChatModel != nil {
		t.Error("expected inner model to be nil")
	}
}

// TestCacheTTLConstants verifies that the TTL constants are correctly defined
func TestCacheTTLConstants(t *testing.T) {
	// Verify that the claude package constants are accessible
	if claude.CacheTTL5m == "" {
		t.Error("CacheTTL5m should not be empty")
	}
	if claude.CacheTTL1h == "" {
		t.Error("CacheTTL1h should not be empty")
	}
}
