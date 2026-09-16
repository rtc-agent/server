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

	"github.com/rtc-agent/server/internal/usecase"
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

// compressContext compresses the conversation history by summarizing old
// messages and keeping recent messages unchanged.
//
// This is the CompressContext callback for the summarization middleware.
// It is called when the trigger condition is met (e.g., token count exceeds
// the threshold).
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
//
// compressContext compresses the conversation context by summarizing older messages.
//
// If force is true, compression is performed regardless of token thresholds.
// This is used for manual /compact commands where the user explicitly requests compression.
//
// Compression strategy:
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
	// Try both to handle all call paths.
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		if sidStr := turnagent.SessionIDFromContext(ctx); sidStr != "" {
			if sid, err := uuid.Parse(sidStr); err == nil {
				sessionID = sid
			}
		}
	}

	// summaryMsgID is set when streaming path is taken (LLM fallback).
	// Remains uuid.Nil for session-memory path (no streaming needed).
	var summaryMsgID uuid.UUID
	var summaryFinalized bool

	var (
		summary           string
		err               error
		sessionMemoryUsed bool
	)

	// Try to use session memory for compression (zero API cost)
	summaryPtr, err := h.compressContextWithSessionMemory(ctx, msgs, retentionIndex)
	if err != nil {
		return nil, fmt.Errorf("compress with session memory: %w", err)
	}

	if summaryPtr != nil {
		// Successfully used session memory (zero-cost path)
		summary = *summaryPtr
		sessionMemoryUsed = true
	} else {
		// Fall back to LLM summarization with streaming.
		// Reuse appendStreamChunk — the same implementation used by thinking
		// streaming. It handles: first-chunk message creation, Redis chunk
		// buffering, live channel publish, finalization with RunAndPublish,
		// and Redis cleanup.
		buildSummaryContent := func(text string) (protocol.ContentData, error) {
			return primitives.SummaryContentData([]primitives.SummaryItem{
				{Role: "system", Content: text},
			})
		}

		onChunk := func(chunk string) error {
			return h.appendStreamChunk(ctx, sessionID, uuid.Nil,
				chunk, "", // finishReason="" for intermediate chunks
				&summaryMsgID, &summaryFinalized,
				buildSummaryContent, "summary", nil)
		}

		if retentionIndex == 0 {
			var llmTokenUsage *turnagent.TokenUsage
			summary, llmTokenUsage, err = h.summarizeMessagesStreaming(ctx, msgs, CompactModeFull, onChunk, customInstruction)
			if err != nil {
				if summaryMsgID != uuid.Nil && !summaryFinalized {
					// Mark as failed using the same finalization path
					_ = h.appendStreamChunk(ctx, sessionID, uuid.Nil,
						"", "stream_failed",
						&summaryMsgID, &summaryFinalized,
						buildSummaryContent, "summary", nil)
				}
				return nil, fmt.Errorf("summarize all messages: %w", err)
			}
			_ = llmTokenUsage
		} else {
			oldMsgs := msgs[:retentionIndex]
			var llmTokenUsage *turnagent.TokenUsage
			summary, llmTokenUsage, err = h.summarizeMessagesStreaming(ctx, oldMsgs, CompactModePartial, onChunk, customInstruction)
			if err != nil {
				if summaryMsgID != uuid.Nil && !summaryFinalized {
					_ = h.appendStreamChunk(ctx, sessionID, uuid.Nil,
						"", "stream_failed",
						&summaryMsgID, &summaryFinalized,
						buildSummaryContent, "summary", nil)
				}
				return nil, fmt.Errorf("summarize old messages: %w", err)
			}
			_ = llmTokenUsage
		}

		// Finalize the streaming message with complete content + metadata
		if summaryMsgID != uuid.Nil && !summaryFinalized {
			compressDuration := time.Since(compressStart)
			tokensAfter := estimateTokensAfterCompact(msgs, retentionIndex, summary)
			metadata := &primitives.SummaryMetadata{
				TokensBefore:      tokensBefore,
				TokensAfter:       tokensAfter,
				DurationMs:        compressDuration.Milliseconds(),
				Mode:              compressModeString(retentionIndex),
				SessionMemoryUsed: sessionMemoryUsed,
			}
			// Build a final content builder that includes metadata.
			// appendStreamChunk will call this with the full concatenated text
			// from Redis chunks, then serialize + write to DB + publish.
			finalBuildContent := func(text string) (protocol.ContentData, error) {
				return primitives.SummaryContentDataWithMetadata(
					[]primitives.SummaryItem{{Role: "system", Content: text}},
					metadata,
				)
			}
			_ = h.appendStreamChunk(ctx, sessionID, uuid.Nil,
				"", "stream_finalize",
				&summaryMsgID, &summaryFinalized,
				finalBuildContent, "summary", nil)
		}
	}

	// Mark as persisted to prevent double persistence by OnCompress callback
	if summaryMsgID != uuid.Nil && sessionID != uuid.Nil {
		h.persistedSummaryMsgIDs.Store(sessionID.String(), summaryMsgID.String())
	}

	// If no streaming message was created (session memory path, or message creation failed),
	// fall back to direct persistence
	if summaryMsgID == uuid.Nil {
		if err := h.persistCompressedMessages(ctx, []*schema.Message{
			{Role: schema.User, Content: formatCompactUserMessage(formatCompactSummary(summary))},
		}); err != nil {
			h.logger.Info(ctx, "compress.fallback_persist_error", map[string]any{
				"error": err.Error(),
			})
		}
	}

	// Format the summary as a user message using the template.
	summaryContent := formatCompactUserMessage(formatCompactSummary(summary))

	// Build the compressed message list.
	summaryMsg := &schema.Message{
		Role:    schema.User,
		Content: summaryContent,
	}

	// Preserve system messages from the discarded portion.
	// System messages carry dynamic attachments (AgentPrompt, TodoList,
	// SessionMemory, UserMemory) that are injected by loadMessages.
	// formatMessagesForCompact already skips them when building the
	// summarization prompt (they are meta-instructions, not conversation),
	// but without explicit preservation they would be lost after compression.
	// This ensures the LLM always has access to behavioral rules and
	// persistent context, even after aggressive compression.
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

	// Phase 9: Post-compact file recovery.
	// Attach recently-read file contents (from the discarded portion) as
	// system-reminder messages so the LLM can resume work without
	// re-reading files it was just working with.
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
func (h *helpers) summarizeMessages(ctx context.Context, msgs []*schema.Message, mode CompactMode, customInstruction *string) (string, error) {
	// Build the full prompt with conversation history.
	prompt := buildSummarizePrompt(msgs, mode, customInstruction)

	// Call the LLM to generate the summary using streaming (required for long operations).
	// Disable thinking to save tokens and reduce latency - compression doesn't need reasoning.
	userPrompt := schema.UserMessage(prompt)
	stream, err := h.deps.ChatModel.Stream(ctx, []*schema.Message{userPrompt}, h.noThinkingOptions()...)
	if err != nil {
		return "", fmt.Errorf("chat model stream: %w", err)
	}
	defer stream.Close()

	// Consume the stream to build the complete response.
	var contentBuilder strings.Builder
	var lastMsg *schema.Message

streamLoop:
	for {
		res, timedOut := turnagent.RecvWithTimeout(ctx, stream.Recv, turnagent.StreamIdleTimeout)
		if timedOut {
			stream.Close()
			return "", &turnagent.StreamIdleTimeoutError{
				SessionID: turnagent.SessionIDFromContext(ctx),
				TurnID:    turnagent.TurnIDFromContext(ctx),
				Timeout:   turnagent.StreamIdleTimeout,
			}
		}
		if res.Err != nil {
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				stream.Close()
				return "", res.Err
			}
			if errors.Is(res.Err, io.EOF) {
				break streamLoop
			}
			return "", fmt.Errorf("stream recv: %w", res.Err)
		}
		if res.Msg == nil {
			continue
		}
		lastMsg = res.Msg
		contentBuilder.WriteString(res.Msg.Content)
	}

	content := contentBuilder.String()
	if content == "" {
		return "", fmt.Errorf("chat model returned empty response")
	}

	// Record token usage for the summarization LLM call.
	if lastMsg != nil {
		h.logSummarizeTokenUsage(ctx, lastMsg)
	}

	return content, nil
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
			stream.Close()
			return "", nil, &turnagent.StreamIdleTimeoutError{
				SessionID: turnagent.SessionIDFromContext(ctx),
				TurnID:    turnagent.TurnIDFromContext(ctx),
				Timeout:   turnagent.StreamIdleTimeout,
			}
		}
		if res.Err != nil {
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				stream.Close()
				return "", nil, res.Err
			}
			if res.Err == io.EOF {
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
				h.logger.Info(ctx, "summarize.on_chunk_error", map[string]any{"error": cbErr.Error()})
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

// buildSummarizePrompt constructs the full prompt for summarization.
//
// The prompt consists of:
//  1. The compression template (varies by mode), or the customInstruction if provided.
//  2. Formatted conversation history with tool call details.
//
// If customInstruction is non-nil and non-empty, it replaces the default
// compression prompt template (getCompactPrompt). The conversation history
// is always appended.
func buildSummarizePrompt(msgs []*schema.Message, mode CompactMode, customInstruction *string) string {
	var sb strings.Builder

	// Write the compression prompt template (or custom instruction).
	if customInstruction != nil && *customInstruction != "" {
		sb.WriteString(*customInstruction)
	} else {
		sb.WriteString(getCompactPrompt(mode))
	}
	sb.WriteString("\n\n")

	// Write the conversation history.
	sb.WriteString("# Conversation History\n\n")
	sb.WriteString(formatMessagesForCompact(msgs))

	return sb.String()
}

// formatMessagesForCompact formats messages for the summarization prompt.
//
// Unlike the simple `[role] content` format, this function:
//   - Includes tool call details (name, arguments) for assistant messages.
//   - Includes tool result metadata (tool name, call ID) for tool messages.
//   - Includes reasoning content for assistant messages.
//   - Preserves the chronological flow of the conversation.
func formatMessagesForCompact(msgs []*schema.Message) string {
	var sb strings.Builder

	for i, msg := range msgs {
		role := string(msg.Role)
		if role == "" {
			role = "unknown"
		}

		switch msg.Role {
		case schema.Assistant:
			// Write assistant message content.
			if msg.Content != "" {
				fmt.Fprintf(&sb, "## %s (turn %d)\n\n", role, i+1)
				sb.WriteString(msg.Content)
				sb.WriteString("\n\n")
			}

			// Write reasoning content if present.
			if msg.ReasoningContent != "" {
				fmt.Fprintf(&sb, "<thinking>\n%s\n</thinking>\n\n", msg.ReasoningContent)
			}

			// Write tool calls if present.
			if len(msg.ToolCalls) > 0 {
				sb.WriteString("### Tool Calls\n\n")
				for _, tc := range msg.ToolCalls {
					fmt.Fprintf(&sb, "- **%s** (id: `%s`)\n", tc.Function.Name, tc.ID)
					if tc.Function.Arguments != "" {
						// Format arguments as a code block for readability.
						fmt.Fprintf(&sb, "  ```json\n  %s\n  ```\n", tc.Function.Arguments)
					}
				}
				sb.WriteString("\n")
			}

		case schema.Tool:
			// Write tool result.
			fmt.Fprintf(&sb, "## %s (result for %s, id: `%s`)\n\n", role, msg.ToolName, msg.ToolCallID)
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")

		case schema.User:
			// Write user message.
			if msg.Content != "" {
				fmt.Fprintf(&sb, "## %s (turn %d)\n\n", role, i+1)
				sb.WriteString(msg.Content)
				sb.WriteString("\n\n")
			}

		case schema.System:
			// Skip system messages in the summary (they're meta-instructions).
			continue

		default:
			// Fallback for unknown roles.
			fmt.Fprintf(&sb, "## %s\n\n%s\n\n", role, msg.Content)
		}
	}

	return sb.String()
}

// logSummarizeTokenUsage emits a structured log line for a summarization LLM
// call. Metrics are handled by the eino callback (see token_callback.go); this
// function only logs the supplementary fields (cached_tokens, finish_reason).
func (h *helpers) logSummarizeTokenUsage(ctx context.Context, resp *schema.Message) {
	if resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return
	}

	usage := resp.ResponseMeta.Usage

	sessionID := turnagent.SessionIDFromContext(ctx)
	turnID := turnagent.TurnIDFromContext(ctx)

	h.logger.Info(ctx, "summarize.llm_complete", map[string]any{
		"session_id":    sessionID,
		"turn_id":       turnID,
		"input_tokens":  usage.PromptTokens,
		"output_tokens": usage.CompletionTokens,
		"total_tokens":  usage.TotalTokens,
		"cached_tokens": usage.PromptTokenDetails.CachedTokens,
		"finish_reason": resp.ResponseMeta.FinishReason,
	})
}

// persistCompressedMessages persists only the LLM-generated summary to the
// database after summarization.
//
// This is the OnCompress callback for the summarization middleware. After
// the middleware compresses the messages (via CompressContext), this callback
// is called so the application can persist the compressed form.
//
// Strategy: Only persist the summary text as a single system message.
// The old messages remain in the DB for audit/frontend access. The
// loadMessages function will find the summary message and truncate the
// history at that point.
//
// The first message in `compressed` is expected to be the summary message
// (a user message containing the formatted summary). We persist this as
// a system message with ContentTypeSummary.
func (h *helpers) persistCompressedMessages(ctx context.Context, compressed []*schema.Message) error {
	if len(compressed) == 0 {
		return nil
	}

	// Extract sessionID from context — try both custom key (set by createAgent)
	// and turnagent key (set by eino GenInput).
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		if sidStr := turnagent.SessionIDFromContext(ctx); sidStr != "" {
			if sid, err := uuid.Parse(sidStr); err == nil {
				sessionID = sid
			}
		}
	}
	if sessionID == uuid.Nil {
		h.logger.Info(ctx, "persistCompressedMessages.skip_no_session_id", map[string]any{
			"compressed_count": len(compressed),
		})
		return nil
	}

	// Check if we've already persisted the summary message in compressContext
	// (streaming flow). If so, skip to prevent double persistence.
	if _, loaded := h.persistedSummaryMsgIDs.LoadAndDelete(sessionID.String()); loaded {
		h.logger.Info(ctx, "persistCompressedMessages.skip_already_persisted", map[string]any{
			"session_id": sessionID.String(),
		})
		return nil
	}

	// The first message is the summary (created by compressContext).
	// Extract its content as the summary text.
	summaryText := compressed[0].Content

	// Create a single SummaryItem containing the summary.
	contents := []primitives.SummaryItem{
		{
			Role:    "system",
			Content: summaryText,
		},
	}

	// Create the summary message in DB.
	summaryContentData, err := primitives.SummaryContentData(contents)
	if err != nil {
		return fmt.Errorf("create summary content data: %w", err)
	}
	_, err = primitives.CreateMessage(
		ctx, h.deps,
		sessionID,
		nil, // turnID — summary doesn't belong to a specific turn
		protocol.MessageRoleSystem,
		usecase.SystemCreator{},
		summaryContentData,
		protocol.MessageStreamingCompleted,
		"",  // clientID — system-generated
		nil, // no parent message
	)
	if err != nil {
		return fmt.Errorf("create summary message: %w", err)
	}

	h.logger.Info(ctx, "persistCompressedMessages.done", map[string]any{
		"session_id": sessionID.String(),
	})

	// 压缩后回写 current_context_tokens：使用 compressed 消息的实际 token 数。
	// 这会让前端进度条从累计的 TotalTokens 切换到真实的上下文大小。
	if tokensAfter, err := cumulativeTokenCounter(ctx, compressed); err == nil && tokensAfter > 0 {
		if err := h.deps.SessionRepo.Update(ctx, sessionID, map[string]any{
			"current_context_tokens": tokensAfter,
		}); err != nil {
			h.logger.Info(ctx, "persistCompressedMessages.update_context_tokens_failed", map[string]any{
				"session_id": sessionID.String(),
				"error":      err.Error(),
			})
		}
	}

	// Note: we do NOT call tokenEstimator.Invalidate here.
	// For manual compact: ReestimateAfterCompact handles cache management
	// (it reads the pre-compact EWMA saved by processCompactWorker, adjusts by
	// compression ratio, and writes a fresh estimate).
	// For auto-compression: the token callback's Estimate call already wrote
	// a new EWMA to cache. Deleting it here would lose that information and
	// cause the next Estimate to fall back to defaultGrowth.

	return nil
}

// cumulativeTokenCounter estimates the total token count across all messages.
//
// Delegates to the shared implementation in pkg/turn-agent.
// See turnagent.CumulativeTokenCounter for the full algorithm.
func cumulativeTokenCounter(ctx context.Context, messages []*schema.Message) (int, error) {
	return turnagent.CumulativeTokenCounter(ctx, messages)
}

// estimateMessageTokensPrecise estimates token count for a single message
// with high precision (~4 chars per token).
//
// Delegates to the shared implementation in pkg/turn-agent.
// See turnagent.EstimateMessageTokensPrecise for details.
func estimateMessageTokensPrecise(msg *schema.Message) int {
	return turnagent.EstimateMessageTokensPrecise(msg)
}

// =============================================================================
// Streaming compression helpers
// =============================================================================

// estimateTokensAfterCompact estimates the token count after compression.
func estimateTokensAfterCompact(msgs []*schema.Message, retentionIndex int, summaryText string) int {
	// Estimate tokens for the summary
	summaryTokens := len(summaryText) / 4 // Rough estimate: 4 chars per token

	// Estimate tokens for retained messages
	retainedTokens := 0
	if retentionIndex < len(msgs) {
		for _, msg := range msgs[retentionIndex:] {
			retainedTokens += estimateMessageTokensPrecise(msg)
		}
	}

	return summaryTokens + retainedTokens
}

// compressModeString returns the compression mode as a string.
func compressModeString(retentionIndex int) string {
	if retentionIndex == 0 {
		return "full"
	}
	return "partial"
}

// =============================================================================
// Session context helpers
// =============================================================================

// compressContextKey marks a context as originating from a compression LLM call.
// When this key is set, the token callback skips updating CurrentContextTokens
// to avoid double-counting compression overhead.
//
// BUG-08 fix: Compression flow writes CurrentContextTokens twice:
//  1. persistCompressedMessages writes accurate tokensAfter
//  2. token callback writes tokensAfter + fullUsage.TotalTokens (incorrect)
//
// The context mark prevents the second write for compression calls only.
type compressContextKey struct{}

// withCompressContext returns a child context marked as a compression LLM call.
// The token callback checks this mark and skips CurrentContextTokens update.
func withCompressContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, compressContextKey{}, true)
}

// isCompressContext checks if the context is marked as a compression call.
func isCompressContext(ctx context.Context) bool {
	v, _ := ctx.Value(compressContextKey{}).(bool)
	return v
}

type sessionIDKey struct{}

// withSessionID returns a child context carrying the given session ID.
// Used in LoadMessages to propagate sessionID to the summarization
// middleware's callbacks.
func withSessionID(ctx context.Context, sessionID uuid.UUID) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// getSessionIDFromContext reads the session ID from ctx, or returns uuid.Nil
// if unset.
func getSessionIDFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(sessionIDKey{}).(uuid.UUID)
	return id
}
