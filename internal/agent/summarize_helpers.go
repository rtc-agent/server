package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

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
			h.logger.Warn(ctx, "persistCompressedMessages.update_context_tokens_failed", map[string]any{
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
