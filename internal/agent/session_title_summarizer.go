// Package agent - session_title_summarizer.go
// Session title summarizer.
package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/config"
	appmodel "github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/logger"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

//go:embed prompts/session-title-summarize.md
var sessionTitleSummarizePrompt string

// maxConversationText is the maximum conversation text length (in characters).
// Following claude-code implementation, input length is limited to control token consumption.
const maxConversationText = 1000

// SessionTitleSummarizer generates session title summaries.
type SessionTitleSummarizer struct {
	chatModel            einomodel.ToolCallingChatModel
	sessionRepo          repo.SessionRepo
	messageRepo          repo.MessageRepo
	llmConfig            config.LLMConfig
	maxMessages          int // maximum messages for summary generation
	maxTitleLen          int // maximum title length (in characters)
	tokenCallbackHandler callbacks.Handler
}

// NewSessionTitleSummarizer creates a SessionTitleSummarizer.
func NewSessionTitleSummarizer(
	chatModel einomodel.ToolCallingChatModel,
	sessionRepo repo.SessionRepo,
	messageRepo repo.MessageRepo,
	llmConfig config.LLMConfig,
	tokenCallbackHandler callbacks.Handler,
) *SessionTitleSummarizer {
	return &SessionTitleSummarizer{
		chatModel:            chatModel,
		sessionRepo:          sessionRepo,
		messageRepo:          messageRepo,
		llmConfig:            llmConfig,
		maxMessages:          10,
		maxTitleLen:          50,
		tokenCallbackHandler: tokenCallbackHandler,
	}
}

// noThinkingOptions returns options that disable thinking/reasoning.
// Title summaries don't need reasoning; disabling saves tokens.
func (s *SessionTitleSummarizer) noThinkingOptions() []einomodel.Option {
	var opts []einomodel.Option
	switch s.llmConfig.Provider {
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

// SummarizeIfNeeded generates and updates the title for the specified session.
// Only generates a new title when the session title still starts with "(" (i.e., the initial truncated title).
func (s *SessionTitleSummarizer) SummarizeIfNeeded(ctx context.Context, sessionID uuid.UUID) (string, error) {
	if s.chatModel == nil {
		return "", nil // LLM not configured, skip title generation
	}

	// Set sessionID in context so the token callback handler can find it.
	ctx = turnagent.WithSessionID(ctx, sessionID.String())

	// Initialize eino callbacks so the LLM call's token usage is recorded
	// to Session.TotalTokens via the shared token callback handler.
	if s.tokenCallbackHandler != nil {
		ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, s.tokenCallbackHandler)
	}

	// Get session messages (most recent N).
	modelMessages, err := s.messageRepo.ListRecentBySession(ctx, sessionID, s.maxMessages)
	if err != nil {
		return "", fmt.Errorf("list messages: %w", err)
	}
	if len(modelMessages) == 0 {
		return "", nil // no messages, no title needed
	}

	// Extract conversation text (following claude-code implementation).
	conversationText := extractConversationText(modelMessages, s.maxMessages)
	if conversationText == "" {
		return "", nil
	}

	// Generate title.
	title, err := s.generateTitle(ctx, conversationText)
	if err != nil {
		return title, fmt.Errorf("generate title: %w", err)
	}

	if title == "" {
		return title, nil
	}

	logger.Info(ctx, "session_title.summarized",
		zap.String("session_id", sessionID.String()),
		zap.String("title", title))

	return title, nil
}

// extractConversationText extracts conversation text from messages.
// Only takes user and assistant messages, with a total length limit.
func extractConversationText(messages []*appmodel.Message, maxMessages int) string {
	var parts []string
	totalLen := 0

	for i, msg := range messages {
		if i >= maxMessages {
			break
		}
		// Only take user and assistant messages.
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		content := msg.Content
		if content == "" {
			continue
		}
		parts = append(parts, content)
		totalLen += len(content)
		// Early termination: limit reached.
		if totalLen >= maxConversationText {
			break
		}
	}

	text := strings.Join(parts, "\n")
	// Tail slice: preserve the most recent context. Uses rune slicing to avoid
	// truncating in the middle of multi-byte UTF-8 characters.
	runes := []rune(text)
	if len(runes) > maxConversationText {
		text = string(runes[len(runes)-maxConversationText:])
	}
	return text
}

// generateTitle calls the LLM to generate a title.
func (s *SessionTitleSummarizer) generateTitle(ctx context.Context, conversationText string) (string, error) {
	// Build prompt.
	prompt := sessionTitleSummarizePrompt + "\n\n" + conversationText

	// Use Stream to call LLM (long operations require streaming mode).
	// Thinking is disabled to save tokens.
	stream, err := s.chatModel.Stream(ctx, []*schema.Message{
		schema.UserMessage(prompt),
	}, s.noThinkingOptions()...)
	if err != nil {
		return "", fmt.Errorf("chat model stream: %w", err)
	}
	defer stream.Close()

	// Per-read idle timeout to prevent goroutine hangs if the LLM stream stalls.
	// Aligned with consumeStream and summarizeMessagesStreaming which use the same
	// RecvWithTimeout wrapper. Use a short timeout since title generation is a quick
	// operation and the caller already has a 30s overall context deadline.
	const titleStreamIdleTimeout = 30 * time.Second

	// Consume stream to build complete response.
	var contentBuilder strings.Builder
	for {
		res, timedOut := turnagent.RecvWithTimeout(ctx, stream.Recv, titleStreamIdleTimeout)
		if timedOut {
			return "", fmt.Errorf("title stream idle timeout after %s", titleStreamIdleTimeout)
		}
		if res.Err != nil {
			if errors.Is(res.Err, io.EOF) {
				break
			}
			return "", fmt.Errorf("stream recv: %w", res.Err)
		}
		if res.Msg == nil {
			continue
		}
		contentBuilder.WriteString(res.Msg.Content)
	}

	content := contentBuilder.String()
	if content == "" {
		return "", fmt.Errorf("chat model returned empty response")
	}

	// Clean the response.
	title := cleanTitle(content)

	// Truncate overly long titles.
	if len([]rune(title)) > s.maxTitleLen {
		title = string([]rune(title)[:s.maxTitleLen])
	}

	return title, nil
}

// cleanTitle cleans a title string.
func cleanTitle(raw string) string {
	// Remove possible markdown formatting and quotes.
	title := strings.TrimSpace(raw)
	title = strings.Trim(title, "`\"'*")
	title = strings.TrimPrefix(title, "Title:")
	title = strings.TrimPrefix(title, "标题:")
	title = strings.TrimSpace(title)

	// Take the first line.
	if idx := strings.Index(title, "\n"); idx != -1 {
		title = title[:idx]
	}

	return strings.TrimSpace(title)
}
