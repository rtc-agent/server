package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// buildSummarizationMiddleware constructs the summarization middleware for
// context compression.
//
// The middleware is created once in New() and shared across all turns via
// Config.AgentMiddlewares. It is stateless — the per-turn state (messages
// to compress, session identity) is captured in the CompressContext and
// OnCompress closures.
func (h *helpers) buildSummarizationMiddleware() (adk.ChatModelAgentMiddleware, error) {
	mw, err := turnagent.NewSummarizationMiddleware(&turnagent.SummarizationConfig{
		Trigger: &turnagent.TriggerCondition{
			ContextTokens: h.contextTokensLimit,
		},
		TokenCounter:    cumulativeTokenCounter,
		CompressContext: h.compressContext,
		OnCompress:      h.persistCompressedMessages,
		Log:             h.logger,
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
func (h *helpers) compressContext(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
	if !shouldCompressByTokenCount(msgs) {
		return msgs, nil
	}

	config := DefaultRetentionConfig()
	retentionIndex := calculateRetentionIndex(msgs, config)

	// If all messages fit in retention budget, skip compression.
	if retentionIndex >= len(msgs) {
		return msgs, nil
	}

	var (
		summary string
		err     error
	)

	// Try to use session memory for compression (zero API cost)
	summaryPtr, err := h.compressContextWithSessionMemory(ctx, msgs, retentionIndex)
	if err != nil {
		return nil, fmt.Errorf("compress with session memory: %w", err)
	}

	if summaryPtr != nil {
		// Successfully used session memory
		summary = *summaryPtr
	} else {
		// Fall back to LLM summarization
		if retentionIndex == 0 {
			// Full compact: summarize all messages.
			summary, err = h.summarizeMessages(ctx, msgs, CompactModeFull)
			if err != nil {
				return nil, fmt.Errorf("summarize all messages: %w", err)
			}
		} else {
			// Partial compact: summarize older messages, keep recent.
			oldMsgs := msgs[:retentionIndex]
			summary, err = h.summarizeMessages(ctx, oldMsgs, CompactModePartial)
			if err != nil {
				return nil, fmt.Errorf("summarize old messages: %w", err)
			}
		}
	}

	// Format the summary as a user message using the template.
	summaryContent := formatCompactUserMessage(formatCompactSummary(summary))

	// Build the compressed message list.
	summaryMsg := &schema.Message{
		Role:    schema.User,
		Content: summaryContent,
	}

	result := make([]*schema.Message, 0, 1+len(msgs)-retentionIndex)
	result = append(result, summaryMsg)
	if retentionIndex < len(msgs) {
		result = append(result, msgs[retentionIndex:]...)
	}

	// Phase 9: Post-compact file recovery.
	// Attach recently-read file contents (from the discarded portion) as
	// system-reminder messages so the LLM can resume work without
	// re-reading files it was just working with.
	discarded := msgs[:retentionIndex]
	result = appendPostCompactAttachments(ctx, h, result, discarded)

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
func (h *helpers) summarizeMessages(ctx context.Context, msgs []*schema.Message, mode CompactMode) (string, error) {
	// Build the full prompt with conversation history.
	prompt := buildSummarizePrompt(msgs, mode)

	// Call the LLM to generate the summary.
	userPrompt := schema.UserMessage(prompt)
	resp, err := h.deps.ChatModel.Generate(ctx, []*schema.Message{userPrompt})
	if err != nil {
		return "", fmt.Errorf("chat model generate: %w", err)
	}

	if resp == nil || len(resp.Content) == 0 {
		return "", fmt.Errorf("chat model returned empty response")
	}

	// Record token usage for the summarization LLM call.
	h.logSummarizeTokenUsage(ctx, resp)

	return resp.Content, nil
}

// buildSummarizePrompt constructs the full prompt for summarization.
//
// The prompt consists of:
//  1. The compression template (varies by mode).
//  2. Formatted conversation history with tool call details.
func buildSummarizePrompt(msgs []*schema.Message, mode CompactMode) string {
	var sb strings.Builder

	// Write the compression prompt template.
	sb.WriteString(getCompactPrompt(mode))
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

	h.logIfEnabled(ctx, "summarize.llm_complete", map[string]any{
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

	// Extract sessionID from context (injected by createAgent via withSessionID).
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		h.logIfEnabled(ctx, "persistCompressedMessages.skip_no_session_id", map[string]any{
			"compressed_count": len(compressed),
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

	h.logIfEnabled(ctx, "persistCompressedMessages.done", map[string]any{
		"session_id": sessionID.String(),
	})

	return nil
}

// cumulativeTokenCounter estimates the total token count across all messages.
//
// Strategy:
//   - For messages with ResponseMeta.Usage (from LLM responses), use TotalTokens
//     as the context size baseline at that point in the conversation.
//     TotalTokens already includes cache read + cache creation tokens.
//   - For messages without Usage data, estimate via precise estimation that
//     considers Content, ReasoningContent, MultiContent, and ToolCalls.
//   - Walk backwards to find the last message with Usage, use its TotalTokens
//     as the cumulative baseline, then add estimates for newer messages.
//   - If no messages have Usage data at all, fall back to estimating every
//     message from content length.
func cumulativeTokenCounter(_ context.Context, messages []*schema.Message) (int, error) {
	// 1. 从后向前查找最后一条带 Usage 的 assistant 消息作为基线。
	var baseTokens int
	incrementStart := 0

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			usage := msg.ResponseMeta.Usage
			// TotalTokens 已经是准确值（包含 cache read + cache creation）
			if usage.TotalTokens > 0 {
				baseTokens = usage.TotalTokens
				incrementStart = i + 1
				break
			}
		}
	}

	// 2. 累加基线之后新增消息的估算 token（使用精确估算）。
	var estimated int
	for _, msg := range messages[incrementStart:] {
		estimated += estimateMessageTokensPrecise(msg)
	}

	return baseTokens + estimated, nil
}

// estimateMessageTokensPrecise estimates token count for a single message
// with high precision (~4 chars per token).
//
// Unlike the simpler estimateTokens, this function considers:
//   - Content and ReasoningContent
//   - UserInputMultiContent (user input multimodal content)
//   - AssistantGenMultiContent (model output multimodal content)
//   - ToolCalls (function name, arguments, and ID)
//   - Tool result metadata (ToolCallID, ToolName)
func estimateMessageTokensPrecise(msg *schema.Message) int {
	if msg == nil {
		return 0
	}

	var charCount int

	// 主内容
	charCount += len(msg.Content)
	charCount += len(msg.ReasoningContent)

	// UserInputMultiContent（用户输入多模态内容）
	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			charCount += len(part.Text)
		}
		// 图片等多模态内容按固定 token 估算（约 250 tokens）
		if part.Type == schema.ChatMessagePartTypeImageURL {
			charCount += 1000
		}
	}

	// AssistantGenMultiContent（模型输出多模态内容）
	for _, part := range msg.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			charCount += len(part.Text)
		} else if part.Type == schema.ChatMessagePartTypeReasoning && part.Reasoning != nil {
			charCount += len(part.Reasoning.Text)
		}
	}

	// ToolCalls（工具调用）
	for _, tc := range msg.ToolCalls {
		charCount += len(tc.Function.Name)
		charCount += len(tc.Function.Arguments)
		charCount += len(tc.ID) // tool call ID
	}

	// Tool 结果消息
	if msg.Role == schema.Tool {
		charCount += len(msg.ToolCallID)
		charCount += len(msg.ToolName)
	}

	return charCount / 4 // 4 chars per token
}

// estimateTokens estimates token count for a single message (~4 chars/token).
// This is the simpler version kept for backward compatibility.
// For more precise estimation, use estimateMessageTokensPrecise.
func estimateTokens(msg *schema.Message) int {
	if msg == nil {
		return 0
	}
	return len(msg.Content)/4 + len(msg.ReasoningContent)/4
}

// =============================================================================
// Session context helpers
// =============================================================================

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
