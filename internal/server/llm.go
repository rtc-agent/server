package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/config"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// NewChatModel 根据配置创建 ChatModel（支持 Claude 和 OpenAI）
func NewChatModel(cfg *config.Config, metrics *turnagent.PrometheusMetrics) (model.ToolCallingChatModel, error) {
	return newChatModel(context.Background(), &cfg.LLM, cfg.Log.LLMPayload, metrics)
}

// newChatModel 根据配置创建 ChatModel（支持 Claude 和 OpenAI）
// observability transport 始终注入，保证 metrics 统计 100% 覆盖。
// llmPayloadLog 启用时额外记录完整 API 请求/响应到 logs/llm-payload.log。
func newChatModel(ctx context.Context, cfg *config.LLMConfig, llmPayloadLog bool, metrics *turnagent.PrometheusMetrics) (model.ToolCallingChatModel, error) {
	if cfg.Provider == "" {
		return nil, fmt.Errorf("llm.provider not configured")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("llm.model not configured")
	}

	// 创建 metrics observer（如果 metrics 不为 nil）
	var observer LLMResponseObserver
	if metrics != nil {
		observer = NewHTTPMetricsObserver(metrics)
	}

	// 始终创建 observability HTTP client，确保 metrics 统计全覆盖
	httpClient := newObservabilityHTTPClient(llmPayloadLog, observer)

	switch cfg.Provider {
	case "claude":
		return newClaudeModel(ctx, cfg, httpClient)
	case "openai":
		return newOpenAIModel(ctx, cfg, httpClient)
	default:
		return nil, fmt.Errorf("unsupported llm.provider: %s (supported: claude, openai)", cfg.Provider)
	}
}

// newClaudeModel 创建 Claude 模型
func newClaudeModel(ctx context.Context, cfg *config.LLMConfig, httpClient *http.Client) (model.ToolCallingChatModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("llm.api_key required for claude provider")
	}
	if cfg.MaxTokens <= 0 {
		return nil, fmt.Errorf("llm.max_tokens required for claude provider (must be > 0)")
	}

	claudeCfg := &claude.Config{
		APIKey:           cfg.APIKey,
		Model:            cfg.Model,
		MaxTokens:        cfg.MaxTokens,
		ThinkingConfig:   new(anthropic.ThinkingConfigParamOfEnabled(cfg.ThinkingBudgetTokens)),
		AutoCacheControl: &claude.CacheControl{}, // 启用 Anthropic prompt caching，降低 60-80% 成本
	}

	// BaseURL 可选
	if cfg.BaseURL != "" {
		claudeCfg.BaseURL = &cfg.BaseURL
	}

	// HTTPClient 始终注入 observability transport（用于 metrics 统计）
	claudeCfg.HTTPClient = httpClient

	return claude.NewChatModel(ctx, claudeCfg)
}

// newOpenAIModel 创建 OpenAI 模型
func newOpenAIModel(ctx context.Context, cfg *config.LLMConfig, httpClient *http.Client) (model.ToolCallingChatModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("llm.api_key required for openai provider")
	}

	// Map reasoning effort string to OpenAI constant
	reasoningEffort := openai.ReasoningEffortLevelMedium
	switch cfg.ReasoningEffort {
	case "low":
		reasoningEffort = openai.ReasoningEffortLevelLow
	case "high":
		reasoningEffort = openai.ReasoningEffortLevelHigh
	}

	openaiCfg := &openai.ChatModelConfig{
		APIKey:          cfg.APIKey,
		BaseURL:         cfg.BaseURL,
		Model:           cfg.Model,
		Timeout:         cfg.Timeout,
		ReasoningEffort: reasoningEffort,
		ExtraFields: map[string]any{
			"chat_template_kwargs": map[string]any{
				"enable_thinking": true,
			},
		},
	}

	// MaxCompletionTokens 对应 Claude 的 MaxTokens
	if cfg.MaxTokens > 0 {
		openaiCfg.MaxCompletionTokens = &cfg.MaxTokens
	}

	// Temperature 可选
	if cfg.Temperature != nil {
		openaiCfg.Temperature = cfg.Temperature
	}

	// HTTPClient 始终注入 observability transport（用于 metrics 统计）
	openaiCfg.HTTPClient = httpClient

	return openai.NewChatModel(ctx, openaiCfg)
}
