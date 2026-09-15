package server

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// =============================================================================
// Strategic Cache Breakpoints - Claude Provider Adapter
// =============================================================================
//
// This adapter applies strategic cache breakpoints to protect stable content
// from invalidation caused by microcompact or tool result budget modifications.
//
// Architecture:
//   - turnagent.Message.CacheBreakpoint is set by setCacheBreakpoints() in data_context.go
//   - toEinoMessage() converts it to Extra[ExtraKeyCacheBreakpoint]
//   - applyClaudeCacheBreakpoints() recognizes the Extra key and calls
//     claude.SetMessageCacheControl() to set the actual breakpoint
//   - claudeChatModelWrapper intercepts Generate/Stream calls to apply breakpoints
//
// Why a wrapper?
//   - pkg/turn-agent is provider-agnostic and cannot import claude adapter
//   - The wrapper applies Claude-specific cache control at the provider boundary
//   - All ChatModel calls (Generate/Stream) automatically get breakpoints

// claudeChatModelWrapper wraps a ChatModel to automatically apply strategic
// cache breakpoints before each API call.
type claudeChatModelWrapper struct {
	model.ToolCallingChatModel
}

// Generate applies strategic cache breakpoints and delegates to the wrapped model.
func (w *claudeChatModelWrapper) Generate(
	ctx context.Context,
	msgs []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {
	msgs = applyClaudeCacheBreakpoints(msgs)
	return w.ToolCallingChatModel.Generate(ctx, msgs, opts...)
}

// Stream applies strategic cache breakpoints and delegates to the wrapped model.
func (w *claudeChatModelWrapper) Stream(
	ctx context.Context,
	msgs []*schema.Message,
	opts ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	msgs = applyClaudeCacheBreakpoints(msgs)
	return w.ToolCallingChatModel.Stream(ctx, msgs, opts...)
}

// WithTools preserves the wrapper when creating a tool-enabled model.
func (w *claudeChatModelWrapper) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := w.ToolCallingChatModel.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &claudeChatModelWrapper{inner}, nil
}

// applyClaudeCacheBreakpoints identifies messages marked with ExtraKeyCacheBreakpoint
// and applies the actual cache breakpoint using claude.SetMessageCacheControl().
//
// IMPORTANT: SetMessageCacheControl returns a NEW pointer (it creates a copy).
// We MUST capture the return value, otherwise the breakpoint is not set (no-op).
//
// TTL mapping:
//   - "1h" → claude.CacheTTL1h (for summary boundary, long-lived stable content)
//   - "5m" or empty → claude.CacheTTL5m (default, for latest conversation context)
func applyClaudeCacheBreakpoints(msgs []*schema.Message) []*schema.Message {
	for i, msg := range msgs {
		if msg.Extra == nil {
			continue
		}

		isBreakpoint, ok := msg.Extra[turnagent.ExtraKeyCacheBreakpoint].(bool)
		if !ok || !isBreakpoint {
			continue
		}

		// Determine TTL from Extra, default to 5m
		ttl := claude.CacheTTL5m
		if ttlStr, ok := msg.Extra[turnagent.ExtraKeyCacheBreakpointTTL].(string); ok {
			if ttlStr == "1h" {
				ttl = claude.CacheTTL1h
			}
		}

		// CRITICAL: Capture the return value. SetMessageCacheControl creates a copy.
		msgs[i] = claude.SetMessageCacheControl(msg, &claude.CacheControl{TTL: ttl})
	}
	return msgs
}
