// Package turnagent — summarize_middleware.go
//
// # Bridge component
//
// This file provides a ChatModelAgentMiddleware that compresses conversation
// history when it grows too long. It is a direct port of the middleware in
// pkg/turn-loop, adapted for the turn-agent package.
//
// The middleware operates on eino's *schema.Message inside the agent's state.
// This is intentional: the middleware is a bridge between the application
// (which owns the messages and the compression strategy) and eino's agent
// lifecycle (which holds the messages in state). The application does not
// interact with *schema.Message directly — it only injects the middleware
// via Config.AgentMiddlewares and implements the CompressContext / OnCompress
// callbacks, which also use *schema.Message as the transport type.
//
// In other words, this middleware is the one place in the public API where
// eino's flow types are visible to the application. The rationale is that
// context compression is inherently an eino-agent-lifecycle concern, so
// hiding the types would require a parallel abstraction with no practical
// benefit. All other turn-agent entry points use pkg-level types.

package turnagent

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// =============================================================================
// Callback types
// =============================================================================

// TokenCounterFunc calculates the token count for a slice of messages.
type TokenCounterFunc func(ctx context.Context, messages []*schema.Message) (int, error)

// CompressContextFunc is called when token usage exceeds the threshold. It
// receives the current messages and must return compressed messages that fit
// within the context window. This is a blocking call — the middleware waits
// for it to complete.
type CompressContextFunc func(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error)

// CompressionMetricsAttrs describes a single compression event for metrics
// recording. Mirrors the attributes recorded by the turn-loop version.
type CompressionMetricsAttrs struct {
	Trigger      string // "threshold" (currently the only trigger kind)
	TokensBefore int
	TokensAfter  int
	DurationMs   int64
}

// RecordCompressionFunc records a single compression event. Optional — if nil,
// no metrics are emitted.
type RecordCompressionFunc func(ctx context.Context, attrs CompressionMetricsAttrs)

// =============================================================================
// Trigger condition
// =============================================================================

// TriggerCondition specifies when summarization should be activated.
// Summarization triggers if ANY of the set conditions is met.
type TriggerCondition struct {
	// ContextTokens triggers summarization when the total token count
	// exceeds this threshold.
	ContextTokens int
	// ContextMessages triggers summarization when the total messages count
	// exceeds this threshold.
	ContextMessages int
}

// DefaultContextTokenThreshold is the default token threshold for triggering
// summarization when SummarizationConfig.Trigger is nil.
//
// The value (160_000) is chosen to fit within the context windows of common
// large LLMs (e.g., 200k tokens) while leaving headroom for the model's
// output tokens and a safety margin. Override by setting Trigger.ContextTokens
// explicitly.
const DefaultContextTokenThreshold = 160_000

// =============================================================================
// Config
// =============================================================================

// SummarizationConfig defines the configuration for the summarization
// middleware.
type SummarizationConfig struct {
	// Trigger specifies the conditions that activate summarization.
	// Optional. Defaults to DefaultContextTokenThreshold (160k tokens).
	Trigger *TriggerCondition

	// TokenCounter calculates the token count for a slice of messages.
	// Optional. Defaults to an estimator that uses the last assistant
	// message's ResponseMeta.Usage.TotalTokens as a baseline and estimates
	// incremental messages at ~4 chars/token.
	TokenCounter TokenCounterFunc

	// CompressContext is called when trigger conditions are met (required).
	// This is a BLOCKING call — the middleware waits for it to complete.
	// The callback receives the current messages and must return compressed
	// messages that fit within the context window.
	CompressContext CompressContextFunc

	// OnCompress is called after compression succeeds (optional). The
	// consumer uses this to persist the compressed messages to their
	// storage (e.g., update the message history for cross-turn compression).
	OnCompress func(ctx context.Context, compressed []*schema.Message) error

	// Log is an optional logger. If nil, no logs are emitted.
	//
	// The same Logger implementation can be shared with Config.Logger —
	// the middleware uses the same interface so a single logger instance
	// covers both the turn-agent runtime and the summarization middleware.
	Log Logger

	// RecordCompression is an optional metrics callback. If nil, no metrics
	// are emitted.
	RecordCompression RecordCompressionFunc
}

// =============================================================================
// Middleware
// =============================================================================

// summarizationMiddleware implements adk.ChatModelAgentMiddleware for context
// compression.
type summarizationMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	cfg *SummarizationConfig
}

// NewSummarizationMiddleware creates a middleware that automatically
// compresses conversation history when trigger conditions are met.
//
// The middleware hooks into two points of the eino ChatModelAgent lifecycle:
//
//   - BeforeModelRewriteState: checks trigger conditions and, if any are met,
//     calls CompressContext to rewrite state.Messages in place (within-turn
//     compression). After successful compression, OnCompress is invoked so
//     the consumer can persist the compressed form (cross-turn compression).
//
// To use, construct the middleware and add it to Config.AgentMiddlewares:
//
//	mw, err := turnagent.NewSummarizationMiddleware(&turnagent.SummarizationConfig{
//	    Trigger: &turnagent.TriggerCondition{ContextTokens: 25000},
//	    CompressContext: func(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
//	        return callLLMToSummarize(ctx, msgs)
//	    },
//	    OnCompress: func(ctx context.Context, compressed []*schema.Message) error {
//	        return persistMessages(ctx, sessionID, compressed)
//	    },
//	})
//	if err != nil {
//	    return err
//	}
//	cfg := turnagent.Config{
//	    AgentMiddlewares: []adk.ChatModelAgentMiddleware{mw},
//	    // ...
//	}
//
// The middleware is opt-in. If Config.AgentMiddlewares is empty or does not
// include this middleware, no summarization occurs.
//
// Returns an error if cfg is nil or cfg.CompressContext is nil (required).
func NewSummarizationMiddleware(cfg *SummarizationConfig) (adk.ChatModelAgentMiddleware, error) {
	if cfg == nil {
		return nil, fmt.Errorf("turnagent: SummarizationConfig must not be nil")
	}
	if cfg.CompressContext == nil {
		return nil, fmt.Errorf("turnagent: SummarizationConfig.CompressContext is required")
	}

	if cfg.Log != nil {
		triggerDesc := "default(160k tokens)"
		if cfg.Trigger != nil {
			if cfg.Trigger.ContextTokens > 0 {
				triggerDesc = fmt.Sprintf("tokens=%d", cfg.Trigger.ContextTokens)
			}
			if cfg.Trigger.ContextMessages > 0 {
				if triggerDesc != "default(160k tokens)" {
					triggerDesc += ","
				} else {
					triggerDesc = ""
				}
				triggerDesc += fmt.Sprintf("messages=%d", cfg.Trigger.ContextMessages)
			}
		}
		cfg.Log.Debug(context.Background(), "summarize_middleware.created", map[string]any{
			"trigger":           triggerDesc,
			"has_token_counter": cfg.TokenCounter != nil,
			"has_on_compress":   cfg.OnCompress != nil,
			"has_log":           cfg.Log != nil,
			"has_record":        cfg.RecordCompression != nil,
		})
	}

	return &summarizationMiddleware{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		cfg:                          cfg,
	}, nil
}

// log dispatches a log call to the appropriate Logger method based on the
// level string. Used internally to keep call sites compact — each call site
// specifies the level as a string ("debug"/"info"/"warn"/"error") rather
// than selecting a Logger method explicitly. Unknown levels fall back to
// Logger.Info.
func (m *summarizationMiddleware) log(ctx context.Context, level, msg string, attrs map[string]any) {
	if m.cfg.Log == nil {
		return
	}
	switch level {
	case "debug":
		m.cfg.Log.Debug(ctx, msg, attrs)
	case "info":
		m.cfg.Log.Info(ctx, msg, attrs)
	case "warn":
		m.cfg.Log.Warn(ctx, msg, attrs)
	case "error":
		m.cfg.Log.Error(ctx, msg, attrs)
	default:
		m.cfg.Log.Info(ctx, msg, attrs)
	}
}

// BeforeModelRewriteState is called before each model invocation. It checks
// if compression is needed and compresses the messages if so.
func (m *summarizationMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	mc *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	m.log(ctx, "debug", "compress.before_model_rewrite", map[string]any{
		"messages_count": len(state.Messages),
	})

	triggered, err := m.shouldCompress(ctx, state.Messages)
	if err != nil {
		m.log(ctx, "error", "compress.should_compress_error", map[string]any{
			"error": err.Error(),
		})
		return ctx, state, fmt.Errorf("summarization middleware: failed to check trigger: %w", err)
	}
	if !triggered {
		m.log(ctx, "debug", "compress.not_triggered", map[string]any{
			"messages_count": len(state.Messages),
		})
		return ctx, state, nil
	}

	tokensBefore, _ := m.countTokens(ctx, state.Messages)
	m.log(ctx, "info", "compress.trigger", map[string]any{
		"tokens_before": tokensBefore,
		"trigger":       "threshold",
	})
	compressStart := time.Now()

	compressed, err := m.cfg.CompressContext(ctx, state.Messages)
	if err != nil {
		return ctx, state, fmt.Errorf("summarization middleware: compression failed: %w", err)
	}

	compressDuration := time.Since(compressStart)
	tokensBeforeFinal, _ := m.countTokens(ctx, state.Messages)
	tokensAfter, _ := m.countTokens(ctx, compressed)
	m.log(ctx, "info", "compress.done", map[string]any{
		"tokens_before": tokensBeforeFinal,
		"tokens_after":  tokensAfter,
		"duration_ms":   compressDuration.Milliseconds(),
	})
	if m.cfg.RecordCompression != nil {
		m.cfg.RecordCompression(ctx, CompressionMetricsAttrs{
			Trigger:      "threshold",
			TokensBefore: tokensBeforeFinal,
			TokensAfter:  tokensAfter,
			DurationMs:   compressDuration.Milliseconds(),
		})
	}

	state.Messages = compressed

	m.log(ctx, "debug", "compress.state_updated", map[string]any{
		"compressed_messages_count": len(compressed),
		"original_messages_count":   len(state.Messages),
	})

	if m.cfg.OnCompress != nil {
		m.log(ctx, "debug", "compress.on_compress_calling", map[string]any{})
		if err := m.cfg.OnCompress(ctx, compressed); err != nil {
			m.log(ctx, "error", "compress.on_compress_error", map[string]any{
				"error": err.Error(),
			})
			return ctx, state, fmt.Errorf("summarization middleware: OnCompress callback failed: %w", err)
		}
		m.log(ctx, "debug", "compress.on_compress_done", map[string]any{})
	}

	m.log(ctx, "debug", "compress.complete", map[string]any{
		"messages_count": len(compressed),
	})

	return ctx, state, nil
}

// =============================================================================
// Internals
// =============================================================================

// shouldCompress checks if any trigger condition is met.
func (m *summarizationMiddleware) shouldCompress(ctx context.Context, messages []*schema.Message) (bool, error) {
	if m.cfg.Trigger == nil {
		tokens, err := m.countTokens(ctx, messages)
		if err != nil {
			return false, err
		}
		triggered := tokens >= DefaultContextTokenThreshold
		m.log(ctx, "debug", "compress.should_compress_check", map[string]any{
			"trigger":   "default_160k_tokens",
			"tokens":    tokens,
			"threshold": DefaultContextTokenThreshold,
			"triggered": triggered,
			"messages":  len(messages),
		})
		return triggered, nil
	}

	if m.cfg.Trigger.ContextMessages > 0 && len(messages) >= m.cfg.Trigger.ContextMessages {
		m.log(ctx, "debug", "compress.should_compress_check", map[string]any{
			"trigger":        "message_count",
			"messages_count": len(messages),
			"threshold":      m.cfg.Trigger.ContextMessages,
			"triggered":      true,
		})
		return true, nil
	}

	if m.cfg.Trigger.ContextTokens > 0 {
		tokens, err := m.countTokens(ctx, messages)
		if err != nil {
			return false, err
		}
		triggered := tokens >= m.cfg.Trigger.ContextTokens
		m.log(ctx, "debug", "compress.should_compress_check", map[string]any{
			"trigger":   "token_count",
			"tokens":    tokens,
			"threshold": m.cfg.Trigger.ContextTokens,
			"triggered": triggered,
			"messages":  len(messages),
		})
		return triggered, nil
	}

	m.log(ctx, "debug", "compress.should_compress_check", map[string]any{
		"trigger":   "no_condition_set",
		"triggered": false,
	})
	return false, nil
}

// countTokens uses the configured token counter or the default estimator.
func (m *summarizationMiddleware) countTokens(ctx context.Context, messages []*schema.Message) (int, error) {
	if m.cfg.TokenCounter != nil {
		m.log(ctx, "debug", "compress.count_tokens", map[string]any{
			"counter":  "custom",
			"messages": len(messages),
		})
		return m.cfg.TokenCounter(ctx, messages)
	}
	m.log(ctx, "debug", "compress.count_tokens", map[string]any{
		"counter":  "default_estimator",
		"messages": len(messages),
	})
	return defaultTokenCounter(ctx, messages)
}

// defaultTokenCounter estimates token count using:
//   - Last assistant message's ResponseMeta.Usage.TotalTokens as baseline
//   - ~4 chars per token for other messages
func defaultTokenCounter(_ context.Context, messages []*schema.Message) (int, error) {
	var baseTokens, incrementStart int

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == schema.Assistant && msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			baseTokens = msg.ResponseMeta.Usage.TotalTokens
			incrementStart = i + 1
			break
		}
	}

	var incrementTokens int
	for _, msg := range messages[incrementStart:] {
		incrementTokens += estimateMessageTokens(msg)
	}

	return baseTokens + incrementTokens, nil
}

// estimateMessageTokens estimates token count for a single message (~4 chars
// per token).
func estimateMessageTokens(msg *schema.Message) int {
	if msg == nil {
		return 0
	}

	var charCount int

	charCount += len(msg.Content)

	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			charCount += len(part.Text)
		}
	}
	for _, part := range msg.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			charCount += len(part.Text)
		} else if part.Type == schema.ChatMessagePartTypeReasoning && part.Reasoning != nil {
			charCount += len(part.Reasoning.Text)
		}
	}

	charCount += len(msg.ReasoningContent)

	for _, tc := range msg.ToolCalls {
		charCount += len(tc.Function.Name) + len(tc.Function.Arguments)
	}

	return charCount / 4
}
