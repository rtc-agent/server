package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// noThinkingOptions returns model options that disable thinking/reasoning
// for the configured LLM provider. Used for compression tasks where
// extended thinking is unnecessary and wasteful.
func (h *helpers) noThinkingOptions() []model.Option {
	var opts []model.Option
	switch h.deps.LLMConfig.Provider {
	case "claude":
		opts = append(opts, einoclaude.WithThinkingConfig(
			&anthropic.ThinkingConfigParamUnion{
				OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
			},
		))
	case "openai":
		opts = append(opts, einoopenai.WithExtraFields(map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
		}))
	}
	return opts
}

// buildSummarizationMiddleware constructs the summarization middleware for
// context compression.
//
// The middleware is created once in New() and shared across all turns via
// Config.AgentMiddlewares. It is stateless — the per-turn state (messages
// to compress, session identity) is captured in the CompressContext and
// OnCompress closures.
func (h *helpers) buildSummarizationMiddleware() (adk.ChatModelAgentMiddleware, error) {
	// Calculate actual trigger threshold: context_tokens_limit - auto_compact_buffer_tokens
	// This ensures compression triggers before hitting the hard limit.
	actualTriggerThreshold := h.contextTokensLimit - h.autoCompactBufferTokens
	if actualTriggerThreshold <= 0 {
		// Fallback to 80% of contextTokensLimit when configuration is invalid
		// This prevents threshold=1 which would cause compression on every turn
		actualTriggerThreshold = int(float64(h.contextTokensLimit) * 0.8)
		h.logger.Info(context.Background(), "summarize.threshold_fallback", map[string]any{
			"context_tokens_limit":     h.contextTokensLimit,
			"auto_compact_buffer":      h.autoCompactBufferTokens,
			"actual_trigger_threshold": actualTriggerThreshold,
		})
	}

	h.logger.Info(context.Background(), "summarize.middleware_config", map[string]any{
		"context_tokens_limit":     h.contextTokensLimit,
		"auto_compact_buffer":      h.autoCompactBufferTokens,
		"actual_trigger_threshold": actualTriggerThreshold,
	})

	mw, err := turnagent.NewSummarizationMiddleware(&turnagent.SummarizationConfig{
		Trigger: &turnagent.TriggerCondition{
			ContextTokens: actualTriggerThreshold,
		},
		TokenCounter: cumulativeTokenCounter,
		CompressContext: func(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
			return h.compressContext(ctx, msgs, nil, false) // automatic compression: no custom instruction, not forced
		},
		OnCompress: h.persistCompressedMessages,
		Log:        h.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build summarization middleware: %w", err)
	}
	return mw, nil
}

// finalizeSummaryStreamOnError marks a streaming summary message as failed
// when the LLM summarization call returns an error. This ensures the frontend
// observes the failure instead of seeing a permanently "streaming" message.
//
// Extracted from compressContext to eliminate duplication between the full
// and partial compression error paths.
func (h *helpers) finalizeSummaryStreamOnError(
	ctx context.Context,
	sessionID uuid.UUID,
	summaryMsgID *uuid.UUID,
	summaryFinalized *bool,
	buildSummaryContent func(string) (protocol.ContentData, error),
) {
	if *summaryMsgID == uuid.Nil || *summaryFinalized {
		return
	}
	if chunkErr := h.appendStreamChunk(ctx, sessionID, uuid.Nil,
		"", "stream_failed",
		summaryMsgID, summaryFinalized,
		buildSummaryContent, "summary", nil); chunkErr != nil {
		h.logger.Warn(ctx, "summarize.finalize_failed_on_error", map[string]any{
			"error": chunkErr.Error(),
		})
	}
}

// buildCompressedResult assembles the final compressed message slice after
// summarization. It constructs a summary user message, preserves system
// messages from the discarded portion, and appends retained messages.
//
// System messages carry dynamic attachments (AgentPrompt, TodoList,
// SessionMemory, UserMemory) injected by loadMessages. They are meta-
// instructions, not conversation content, so formatMessagesForCompact
// skips them when building the summarization prompt. Without explicit
// preservation, they would be lost after compression. This ensures the
// LLM always has access to behavioral rules and persistent context.
//
// Shared by compressContext and forceCompressContext.
func buildCompressedResult(
	msgs []*schema.Message,
	retentionIndex int,
	summary string,
) []*schema.Message {
	summaryContent := formatCompactUserMessage(formatCompactSummary(summary))
	summaryMsg := &schema.Message{
		Role:    schema.User,
		Content: summaryContent,
	}

	var systemMsgs []*schema.Message
	for _, msg := range msgs[:retentionIndex] {
		if msg.Role == schema.System {
			systemMsgs = append(systemMsgs, msg)
		}
	}

	result := make([]*schema.Message, 0, 1+len(systemMsgs)+len(msgs)-retentionIndex)
	result = append(result, summaryMsg)
	result = append(result, systemMsgs...)
	if retentionIndex < len(msgs) {
		result = append(result, msgs[retentionIndex:]...)
	}
	return result
}

// runCompressionSummary generates the summary text via session memory or LLM
// streaming. Returns the summary, the streaming message ID (uuid.Nil if no
// streaming was used), whether the streaming message was finalized, whether
// session memory was used, and any error.
//
// Extracted from compressContext to reduce its length and isolate the
// session-memory/LLM branching logic.
func (h *helpers) runCompressionSummary(
	ctx context.Context,
	msgs []*schema.Message,
	retentionIndex int,
	sessionID uuid.UUID,
	customInstruction *string,
) (summary string, summaryMsgID uuid.UUID, summaryFinalized bool, sessionMemoryUsed bool, err error) {
	// Try session memory first (zero API cost)
	summaryPtr := h.compressContextWithSessionMemory(ctx)
	if summaryPtr != nil {
		return *summaryPtr, uuid.Nil, false, true, nil
	}

	// Fall back to LLM summarization with streaming.
	buildSummaryContent := func(text string) (protocol.ContentData, error) {
		return primitives.SummaryContentData([]primitives.SummaryItem{
			{Role: "system", Content: text},
		})
	}

	onChunk := func(chunk string) error {
		return h.appendStreamChunk(ctx, sessionID, uuid.Nil,
			chunk, "",
			&summaryMsgID, &summaryFinalized,
			buildSummaryContent, "summary", nil)
	}

	var mode CompactMode
	var inputMsgs []*schema.Message
	if retentionIndex == 0 {
		mode = CompactModeFull
		inputMsgs = msgs
	} else {
		mode = CompactModePartial
		inputMsgs = msgs[:retentionIndex]
	}

	var llmTokenUsage *turnagent.TokenUsage
	summary, llmTokenUsage, err = h.summarizeMessagesStreaming(ctx, inputMsgs, mode, onChunk, customInstruction)
	if err != nil {
		h.finalizeSummaryStreamOnError(ctx, sessionID, &summaryMsgID, &summaryFinalized, buildSummaryContent)
		modeStr := "all messages"
		if retentionIndex != 0 {
			modeStr = "old messages"
		}
		return "", uuid.Nil, false, false, fmt.Errorf("summarize %s: %w", modeStr, err)
	}

	if llmTokenUsage != nil {
		modeStr := "full"
		if retentionIndex != 0 {
			modeStr = "partial"
		}
		h.logger.Info(ctx, "summarize.llm_token_usage", map[string]any{
			"mode":          modeStr,
			"input_tokens":  llmTokenUsage.InputTokens,
			"output_tokens": llmTokenUsage.OutputTokens,
		})
	}

	return summary, summaryMsgID, summaryFinalized, false, nil
}

// finalizeSummaryStreamWithMetadata writes the final streaming summary message
// with compression metadata (tokens before/after, duration, mode).
//
// Extracted from compressContext to reduce its length.
func (h *helpers) finalizeSummaryStreamWithMetadata(
	ctx context.Context,
	sessionID uuid.UUID,
	summaryMsgID uuid.UUID,
	summaryFinalized *bool,
	msgs []*schema.Message,
	retentionIndex int,
	summary string,
	compressStart time.Time,
	tokensBefore int,
	sessionMemoryUsed bool,
) {
	if summaryMsgID == uuid.Nil || *summaryFinalized {
		return
	}
	compressDuration := time.Since(compressStart)
	tokensAfter := estimateTokensAfterCompact(msgs, retentionIndex, summary)
	metadata := &primitives.SummaryMetadata{
		TokensBefore:      tokensBefore,
		TokensAfter:       tokensAfter,
		DurationMs:        compressDuration.Milliseconds(),
		Mode:              compressModeString(retentionIndex),
		SessionMemoryUsed: sessionMemoryUsed,
	}
	finalBuildContent := func(text string) (protocol.ContentData, error) {
		return primitives.SummaryContentDataWithMetadata(
			[]primitives.SummaryItem{{Role: "system", Content: text}},
			metadata,
		)
	}
	if chunkErr := h.appendStreamChunk(ctx, sessionID, uuid.Nil,
		"", "stream_finalize",
		&summaryMsgID, summaryFinalized,
		finalBuildContent, "summary", nil); chunkErr != nil {
		h.logger.Warn(ctx, "summarize.finalize_failed", map[string]any{
			"error": chunkErr.Error(),
		})
	}
}

// compressContext compresses the conversation history by summarizing old
// messages and keeping recent messages unchanged.
//
// This is the CompressContext callback for the summarization middleware.
// It is called when the trigger condition is met (e.g., token count exceeds
// the threshold).
//
// If force is true, compression is performed regardless of token thresholds.
// This is used for manual /compact commands where the user explicitly requests compression.
//
// Strategy:
//  1. Calculate the retention index using token-based retention config.
//  2. If retention index == 0 → compress all messages (full compact).
//  3. If retention index == len(msgs) → skip compression (nothing to compress).
//  4. Otherwise → compress older messages, keep recent messages (partial compact).
//  5. Return: [system(summary)] + retained_messages.
//
// Session Memory Integration:
//   - Before calling LLM to generate summary, try to use existing session memories.
//   - If session memories exist, use them as the summary (zero API cost).
//   - Otherwise, fall back to LLM summarization.
//
// Streaming flow:
//  1. Create pending summary message (published to topic channel)
//  2. During LLM streaming, publish chunks to live channel in real-time
//  3. On completion, finalize message with metadata and publish to topic channel
func (h *helpers) compressContext(ctx context.Context, msgs []*schema.Message, customInstruction *string, force bool) ([]*schema.Message, error) {
	// BUG-08 fix: Mark context as compression call so token callback skips
	// CurrentContextTokens update (prevents double-counting compression overhead).
	ctx = withCompressContext(ctx)

	// For automatic compression (force=false), check thresholds
	if !force {
		if !shouldCompressByTokenCount(msgs) {
			return msgs, nil
		}
	}

	config := DefaultRetentionConfig()
	retentionIndex := calculateRetentionIndex(msgs, config)

	// For automatic compression, skip if all messages fit in retention budget
	if retentionIndex >= len(msgs) && !force {
		return msgs, nil
	}

	// For forced compression with retentionIndex >= len(msgs),
	// compress as many messages as possible while keeping at least 1 message
	if retentionIndex >= len(msgs) && force {
		// Keep at least 1 message (the most recent one)
		retentionIndex = len(msgs) - 1
		if retentionIndex < 1 {
			// If only 1 message or less, nothing to compress
			return msgs, nil
		}
	}

	compressStart := time.Now()
	tokensBefore, _ := cumulativeTokenCounter(ctx, msgs)

	// Get sessionID once — needed for streaming path and logging.
	// The middleware may receive context from different paths:
	//   - createAgent's withSessionID (custom key)
	//   - eino GenInput's WithSessionID (turnagent key)
	// Try both to handle all call paths. Uses the shared helper to
	// avoid duplicating the fallback logic (see extractSessionIDFromContext).
	sessionID := extractSessionIDFromContext(ctx)

	summary, summaryMsgID, summaryFinalized, sessionMemoryUsed, err := h.runCompressionSummary(ctx, msgs, retentionIndex, sessionID, customInstruction)
	if err != nil {
		return nil, err
	}

	// Mark as persisted to prevent double persistence by OnCompress callback
	if summaryMsgID != uuid.Nil && sessionID != uuid.Nil {
		h.persistedSummaryMsgIDs.Store(sessionID.String(), summaryMsgID.String())
	}

	// If no streaming message was created (session memory path, or message creation failed),
	// fall back to direct persistence
	if summaryMsgID == uuid.Nil {
		if persistErr := h.persistCompressedMessages(ctx, []*schema.Message{
			{Role: schema.User, Content: formatCompactUserMessage(formatCompactSummary(summary))},
		}); persistErr != nil {
			h.logger.Warn(ctx, "compress.fallback_persist_error", map[string]any{
				"error": persistErr.Error(),
			})
		}
	}

	// Finalize the streaming message with complete content + metadata
	h.finalizeSummaryStreamWithMetadata(ctx, sessionID, summaryMsgID, &summaryFinalized, msgs, retentionIndex, summary, compressStart, tokensBefore, sessionMemoryUsed)

	// Phase 9: Post-compact file recovery.
	result := buildCompressedResult(msgs, retentionIndex, summary)
	discarded := msgs[:retentionIndex]
	result = appendPostCompactAttachments(ctx, h, result, discarded)

	compressDuration := time.Since(compressStart)
	tokensAfter := estimateTokensAfterCompact(msgs, retentionIndex, summary)

	h.logger.Info(ctx, "compress.completed", map[string]any{
		"session_id":          sessionID.String(),
		"summary_msg_id":      summaryMsgID.String(),
		"tokens_before":       tokensBefore,
		"tokens_after":        tokensAfter,
		"duration_ms":         compressDuration.Milliseconds(),
		"mode":                compressModeString(retentionIndex),
		"session_memory_used": sessionMemoryUsed,
	})

	return result, nil
}

// summarizeMessages uses the ChatModel to generate a structured summary
// of the given messages using the 9-part prompt template.
//
// The prompt instructs the LLM to produce <analysis> and <summary> blocks.
// The analysis block is stripped in post-processing; only the summary is kept.
//
// The mode parameter selects the appropriate prompt template:
//   - CompactModeFull: For summarizing the entire conversation.
//   - CompactModePartial: For summarizing older messages only.
//   - CompactModePartialUpTo: For summarizing up to a point (continuing session).
//
// If customInstruction is non-nil and non-empty, it overrides the default
// compression prompt template.
//
// This is a thin wrapper around summarizeMessagesStreaming without a chunk
// callback. Both functions share the same stream consumption logic via the
// streaming variant, eliminating duplication.
func (h *helpers) summarizeMessages(ctx context.Context, msgs []*schema.Message, mode CompactMode, customInstruction *string) (string, error) {
	summary, _, err := h.summarizeMessagesStreaming(ctx, msgs, mode, nil, customInstruction)
	return summary, err
}

// summarizeMessagesStreaming generates a summary using streaming LLM calls with real-time callbacks.
// The onChunk callback is invoked for each content chunk received, enabling live progress updates.
// Returns the complete summary text and token usage metadata.
//
// If customInstruction is non-nil and non-empty, it overrides the default
// compression prompt template.
func (h *helpers) summarizeMessagesStreaming(
	ctx context.Context,
	msgs []*schema.Message,
	mode CompactMode,
	onChunk func(chunk string) error,
	customInstruction *string,
) (summary string, tokenUsage *turnagent.TokenUsage, err error) {
	prompt := buildSummarizePrompt(msgs, mode, customInstruction)
	userPrompt := schema.UserMessage(prompt)

	stream, err := h.deps.ChatModel.Stream(ctx, []*schema.Message{userPrompt}, h.noThinkingOptions()...)
	if err != nil {
		return "", nil, fmt.Errorf("chat model stream: %w", err)
	}
	defer stream.Close()

	var contentBuilder strings.Builder
	var lastMsg *schema.Message

streamLoop:
	for {
		res, timedOut := turnagent.RecvWithTimeout(ctx, stream.Recv, turnagent.StreamIdleTimeout)
		if timedOut {
			// stream.Close() handled by defer above.
			return "", nil, &turnagent.StreamIdleTimeoutError{
				SessionID: turnagent.SessionIDFromContext(ctx),
				TurnID:    turnagent.TurnIDFromContext(ctx),
				Timeout:   turnagent.StreamIdleTimeout,
			}
		}
		if res.Err != nil {
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				// stream.Close() handled by defer above.
				return "", nil, res.Err
			}
			if errors.Is(res.Err, io.EOF) {
				break streamLoop
			}
			return "", nil, fmt.Errorf("stream recv: %w", res.Err)
		}
		if res.Msg == nil {
			continue
		}
		lastMsg = res.Msg
		contentBuilder.WriteString(res.Msg.Content)

		// Invoke chunk callback for live updates
		if onChunk != nil && res.Msg.Content != "" {
			if cbErr := onChunk(res.Msg.Content); cbErr != nil {
				// Log but don't fail the stream
				h.logger.Warn(ctx, "summarize.on_chunk_error", map[string]any{"error": cbErr.Error()})
			}
		}
	}

	summary = contentBuilder.String()
	if summary == "" {
		return "", nil, fmt.Errorf("chat model returned empty response")
	}

	// Extract token usage from the last message
	if lastMsg != nil {
		h.logSummarizeTokenUsage(ctx, lastMsg)
		if lastMsg.ResponseMeta != nil && lastMsg.ResponseMeta.Usage != nil {
			tokenUsage = &turnagent.TokenUsage{
				InputTokens:  lastMsg.ResponseMeta.Usage.PromptTokens,
				OutputTokens: lastMsg.ResponseMeta.Usage.CompletionTokens,
				TotalTokens:  lastMsg.ResponseMeta.Usage.TotalTokens,
			}
		}
	}

	return summary, tokenUsage, nil
}
