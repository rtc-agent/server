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
//
// Mapping from old code: this replaces the middleware creation in
// internal/worker/agent.go's createAgent callback. The old code created
// the middleware inline per session; the new code creates it once and
// shares it across all turns.
//
// The middleware uses the turnagent.SummarizationConfig type (from the
// pkg/turn-agent package) rather than the turnloop.SummarizationConfig
// used by the old code. The two are functionally identical; the turnagent
// version is the canonical one going forward.
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
// Strategy: keep the last 10 messages (or 50% of messages, whichever is
// smaller) as "recent", and summarize older messages using the ChatModel.
// The summary is returned as a single system message prepended to the recent
// messages.
//
// Mapping from old code: this is identical to compressContext in
// internal/worker/compress.go.
func (h *helpers) compressContext(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, error) {
	const (
		minMessagesToCompress = 20 // Only compress if we have at least this many messages
		recentMessageCount    = 10 // Keep this many recent messages unchanged
	)

	if len(msgs) < minMessagesToCompress {
		return msgs, nil
	}

	// Split into old (to compress) and recent (to keep).
	recentCount := min(recentMessageCount, len(msgs)/2)
	oldMsgs := msgs[:len(msgs)-recentCount]
	recentMsgs := msgs[len(msgs)-recentCount:]

	// Generate summary of old messages.
	summary, err := h.summarizeMessages(ctx, oldMsgs)
	if err != nil {
		return nil, fmt.Errorf("summarize old messages: %w", err)
	}

	// Prepend summary as a system message.
	summaryMsg := &schema.Message{
		Role:    schema.System,
		Content: summary,
	}

	result := make([]*schema.Message, 0, 1+len(recentMsgs))
	result = append(result, summaryMsg)
	result = append(result, recentMsgs...)

	return result, nil
}

// summarizeMessages uses the ChatModel to generate a concise summary of the
// given messages. The summary is constrained to approximately 500 words to
// ensure it fits within the context budget while preserving key information.
//
// Mapping from old code: this is the direct equivalent of summarizeMessages
// in internal/worker/summarize_messages.go. The implementation is identical:
// build a prompt, concatenate messages with role prefixes, call ChatModel.
//
// Note: the default prompt is written in English to optimize LLM
// comprehension, even though project comments use Chinese. This is intentional
// and improves summary quality.
func (h *helpers) summarizeMessages(ctx context.Context, msgs []*schema.Message) (string, error) {
	// Build a prompt for summarization.
	var sb strings.Builder

	prompt := "Please summarize the following conversation history concisely, " +
		"preserving key information, user preferences, and important context. " +
		"Limit the summary to approximately 500 words.\n\n"
	sb.WriteString(prompt)

	for i, msg := range msgs {
		role := string(msg.Role)
		if role == "" {
			role = "unknown"
		}
		fmt.Fprintf(&sb, "[%s] %s\n", role, msg.Content)
		if i < len(msgs)-1 {
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n\nSummary:")

	// Call the LLM to generate the summary.
	userPrompt := schema.UserMessage(sb.String())
	resp, err := h.deps.ChatModel.Generate(ctx, []*schema.Message{userPrompt})
	if err != nil {
		return "", fmt.Errorf("chat model generate: %w", err)
	}

	if resp == nil || len(resp.Content) == 0 {
		return "", fmt.Errorf("chat model returned empty response")
	}

	// Record token usage for the summarization LLM call.
	// This call bypasses the eino callback chain (it uses the raw ChatModel
	// from deps), so we must record metrics and log manually.
	h.recordSummarizeTokenUsage(ctx, resp)

	return resp.Content, nil
}

// recordSummarizeTokenUsage extracts token usage from a summarization response
// and records it to metrics + structured log.
func (h *helpers) recordSummarizeTokenUsage(ctx context.Context, resp *schema.Message) {
	if resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return
	}

	usage := resp.ResponseMeta.Usage

	sessionID := turnagent.SessionIDFromContext(ctx)
	turnID := turnagent.TurnIDFromContext(ctx)

	if h.metrics != nil {
		h.metrics.RecordLLMCall(ctx, turnagent.LLMCallMetricsAttrs{
			SessionID:    sessionID,
			TurnID:       turnID,
			Model:        "summarize",
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			TotalTokens:  usage.TotalTokens,
		})
	}

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

// persistCompressedMessages persists the compressed message history to the
// database after summarization.
//
// This is the OnCompress callback for the summarization middleware. After
// the middleware compresses the messages (via CompressContext), this callback
// is called so the application can persist the compressed form. This ensures
// the next loadMessages call sees only the summary + recent messages.
//
// Strategy: create a summary message in the DB that captures the compressed
// content. The old messages are NOT deleted — they remain in the DB for
// audit/recovery purposes. The loadMessages function will find the summary
// message and truncate the history at that point.
//
// Mapping from old code: this is similar to persistCompressedMessages in
// internal/worker/compress.go. The old code deleted old messages and created
// a summary; the new code only creates a summary (old messages remain for
// audit purposes, but loadMessages truncates at the summary).
//
// SessionID threading: the summarization middleware does not pass sessionID
// through the context natively. To solve this, createAgent wraps the context
// with sessionID via withSessionID before the agent runs. The middleware's
// callbacks (CompressContext and OnCompress) inherit this context, so
// getSessionIDFromContext can extract the sessionID here.
func (h *helpers) persistCompressedMessages(ctx context.Context, compressed []*schema.Message) error {
	if len(compressed) == 0 {
		return nil
	}

	// Extract sessionID from context (injected by createAgent via withSessionID).
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		// Cannot persist without a sessionID. Log and skip — this should not
		// happen in normal operation because createAgent always injects it.
		h.logIfEnabled(ctx, "persistCompressedMessages.skip_no_session_id", map[string]any{
			"compressed_count": len(compressed),
		})
		return nil
	}

	// Convert compressed messages to SummaryItems for persistence.
	contents := make([]primitives.SummaryItem, 0, len(compressed))
	for _, msg := range compressed {
		item := primitives.SummaryItem{
			Role:    string(msg.Role),
			Content: msg.Content,
		}
		contents = append(contents, item)
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
		"session_id":       sessionID.String(),
		"compressed_count": len(compressed),
	})

	return nil
}

// cumulativeTokenCounter estimates the total token count across all messages.
//
// Strategy:
//   - For messages with ResponseMeta.Usage (from LLM responses), use TotalTokens
//     as the context size baseline at that point in the conversation.
//   - For messages without Usage data, estimate via ~4 chars per token.
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
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil && msg.ResponseMeta.Usage.TotalTokens > 0 {
			baseTokens = msg.ResponseMeta.Usage.TotalTokens
			incrementStart = i + 1
			break
		}
	}

	// 2. 累加基线之后新增消息的估算 token。
	var estimated int
	for _, msg := range messages[incrementStart:] {
		estimated += estimateTokens(msg)
	}

	return baseTokens + estimated, nil
}

// estimateTokens estimates token count for a single message (~4 chars/token).
func estimateTokens(msg *schema.Message) int {
	if msg == nil {
		return 0
	}
	return len(msg.Content)/4 + len(msg.ReasoningContent)/4
}

// =============================================================================
// Session context helpers
// =============================================================================
//
// The summarization middleware's CompressContext and OnCompress callbacks need
// access to the sessionID. The turn-agent package does not thread sessionID
// through the context natively. These helpers provide a mechanism to do so
// via context.WithValue.
//
// createAgent wraps the context with sessionID before passing it to the agent
// constructor. The middleware's callbacks inherit this context and can extract
// the sessionID via getSessionIDFromContext.
//
// Mapping from old code: the old code had sessionID available directly in the
// Manager's methods (compressContext, persistCompressedMessages) because the
// middleware was created per-session with closure-captured state. The new code
// uses context threading because the middleware is shared across all sessions.

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
