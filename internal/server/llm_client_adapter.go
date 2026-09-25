package server

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
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/usecase"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// einoLLMClientAdapter wraps an eino ChatModel to implement webfetch.LLMClient.
// This adapter is created in wire.go and injected into WebFetchManager for LLM extraction.
// It follows the same patterns as SessionTitleSummarizer and summarize.go:
// - Uses Stream() for LLM calls (not Generate)
// - Integrates eino callbacks for token tracking
// - Sets sessionID in context via turnagent.WithSessionID
// - Disables thinking to save tokens
type einoLLMClientAdapter struct {
	chatModel model.ToolCallingChatModel
	llmConfig config.LLMConfig
	deps      *usecase.Dependencies // lazy access to TokenCallbackHandler (set after agent.New())
}

// NewEinoLLMClientAdapter creates an adapter that wraps the eino ChatModel.
// Returns nil if chatModel is nil (LLM not configured).
// deps provides lazy access to TokenCallbackHandler (set inside agent.New()).
func NewEinoLLMClientAdapter(
	chatModel model.ToolCallingChatModel,
	llmConfig config.LLMConfig,
	deps *usecase.Dependencies,
) *einoLLMClientAdapter {
	if chatModel == nil {
		return nil
	}
	return &einoLLMClientAdapter{
		chatModel: chatModel,
		llmConfig: llmConfig,
		deps:      deps,
	}
}

// Generate implements webfetch.LLMClient interface.
// Calls the LLM using Stream() (aligned with SessionTitleSummarizer pattern).
// Token usage is tracked via eino callbacks when tokenCallbackHandler is configured.
func (a *einoLLMClientAdapter) Generate(ctx context.Context, prompt string, maxTokens int, sessionID string) (string, error) {
	if a == nil || a.chatModel == nil {
		return "", fmt.Errorf("LLM client not configured")
	}

	// Enrich context with sessionID for token tracking.
	if sessionID != "" {
		ctx = turnagent.WithSessionID(ctx, sessionID)
	}

	// Initialize eino callbacks for token usage recording.
	// TokenCallbackHandler is set inside agent.New(), so we read it lazily from deps.
	var tokenCallbackHandler callbacks.Handler
	if a.deps != nil {
		tokenCallbackHandler = a.deps.TokenCallbackHandler
	}
	if tokenCallbackHandler != nil {
		ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, tokenCallbackHandler)
	}

	// Build options with thinking disabled (extraction doesn't need reasoning).
	opts := a.noThinkingOptions()

	// Use Stream to call LLM (aligned with SessionTitleSummarizer pattern).
	// Long operations require streaming mode for proper timeout handling.
	stream, err := a.chatModel.Stream(ctx, []*schema.Message{
		schema.UserMessage(prompt),
	}, opts...)
	if err != nil {
		return "", fmt.Errorf("chat model stream: %w", err)
	}
	defer stream.Close()

	// Per-read idle timeout to prevent goroutine hangs if the LLM stream stalls.
	// Aligned with consumeStream and summarizeMessagesStreaming patterns.
	const streamIdleTimeout = 30 * time.Second

	// Consume stream to build complete response.
	var contentBuilder strings.Builder
	for {
		res, timedOut := turnagent.RecvWithTimeout(ctx, stream.Recv, streamIdleTimeout)
		if timedOut {
			return "", fmt.Errorf("LLM extraction stream idle timeout after %s", streamIdleTimeout)
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
		return "", fmt.Errorf("LLM returned empty response")
	}

	return content, nil
}

// noThinkingOptions returns model options that disable thinking/reasoning.
// Follows the same pattern as internal/agent/summarize.go and session_title_summarizer.go.
func (a *einoLLMClientAdapter) noThinkingOptions() []model.Option {
	var opts []model.Option
	switch a.llmConfig.Provider {
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
