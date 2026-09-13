// Package agent - session_title_summarizer.go
// 会话标题摘要生成器
package agent

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino/callbacks"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
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

// maxConversationText 对话文本最大长度（字符数）
// 参考 claude-code 实现，限制输入长度以控制 token 消耗
const maxConversationText = 1000

// SessionTitleSummarizer 会话标题摘要生成器
type SessionTitleSummarizer struct {
	chatModel            einomodel.ToolCallingChatModel
	sessionRepo          repo.SessionRepo
	messageRepo          repo.MessageRepo
	llmConfig            config.LLMConfig
	maxMessages          int // 用于生成摘要的最大消息数
	maxTitleLen          int // 标题最大长度（字符数）
	tokenCallbackHandler callbacks.Handler
}

// NewSessionTitleSummarizer 创建 SessionTitleSummarizer
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

// noThinkingOptions 返回禁用 thinking/reasoning 的选项
// 标题摘要不需要推理，禁用可节省 token
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

// SummarizeIfNeeded 为指定会话生成并更新标题
// 仅当会话标题仍以 "（" 开头（即初始截断标题）时才生成新标题
func (s *SessionTitleSummarizer) SummarizeIfNeeded(ctx context.Context, sessionID uuid.UUID) (string, error) {
	// Set sessionID in context so the token callback handler can find it.
	ctx = turnagent.WithSessionID(ctx, sessionID.String())

	// Initialize eino callbacks so the LLM call's token usage is recorded
	// to Session.TotalTokens via the shared token callback handler.
	if s.tokenCallbackHandler != nil {
		ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, s.tokenCallbackHandler)
	}

	// 获取会话消息（最近的 N 条）
	modelMessages, err := s.messageRepo.ListRecentBySession(ctx, sessionID, s.maxMessages)
	if err != nil {
		return "", fmt.Errorf("list messages: %w", err)
	}
	if len(modelMessages) == 0 {
		return "", nil // 没有消息，无需生成标题
	}

	// 提取对话文本（参考 claude-code 实现）
	conversationText := extractConversationText(modelMessages, s.maxMessages)
	if conversationText == "" {
		return "", nil
	}

	// 生成标题
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

// extractConversationText 从消息中提取对话文本
// 只取 user 和 assistant 消息，限制总长度
func extractConversationText(messages []*appmodel.Message, maxMessages int) string {
	var parts []string
	totalLen := 0

	for i, msg := range messages {
		if i >= maxMessages {
			break
		}
		// 只取用户和助手消息
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		content := msg.Content
		if content == "" {
			continue
		}
		parts = append(parts, content)
		totalLen += len(content)
		// 提前终止：已达上限
		if totalLen >= maxConversationText {
			break
		}
	}

	text := strings.Join(parts, "\n")
	// 尾部切片：保留最近的上下文
	if len(text) > maxConversationText {
		text = text[len(text)-maxConversationText:]
	}
	return text
}

// generateTitle 调用 LLM 生成标题
func (s *SessionTitleSummarizer) generateTitle(ctx context.Context, conversationText string) (string, error) {
	// 构建 prompt
	prompt := sessionTitleSummarizePrompt + "\n\n" + conversationText

	// 使用 Stream 调用 LLM（长时间操作需要流式模式）
	// 禁用 thinking 以节省 token
	stream, err := s.chatModel.Stream(ctx, []*schema.Message{
		schema.UserMessage(prompt),
	}, s.noThinkingOptions()...)
	if err != nil {
		return "", fmt.Errorf("chat model stream: %w", err)
	}
	defer stream.Close()

	// 消费流以构建完整响应
	var contentBuilder strings.Builder
	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			return "", fmt.Errorf("stream recv: %w", recvErr)
		}
		if msg == nil {
			continue
		}
		contentBuilder.WriteString(msg.Content)
	}

	content := contentBuilder.String()
	if content == "" {
		return "", fmt.Errorf("chat model returned empty response")
	}

	// 清理响应
	title := cleanTitle(content)

	// 截断过长的标题
	if len([]rune(title)) > s.maxTitleLen {
		title = string([]rune(title)[:s.maxTitleLen])
	}

	return title, nil
}

// cleanTitle 清理标题
func cleanTitle(raw string) string {
	// 移除可能的 markdown 格式和引号
	title := strings.TrimSpace(raw)
	title = strings.Trim(title, "`\"'*")
	title = strings.TrimPrefix(title, "Title:")
	title = strings.TrimPrefix(title, "标题:")
	title = strings.TrimSpace(title)

	// 取第一行
	if idx := strings.Index(title, "\n"); idx != -1 {
		title = title[:idx]
	}

	return strings.TrimSpace(title)
}
