package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/usecase"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ChatModelFactory creates ChatModel instances with dynamic per-turn overrides.
// Static settings (API key, base URL, timeouts, metrics) are captured at factory
// creation time. Dynamic overrides (model name, max tokens, temperature, etc.)
// are supplied per call.
type ChatModelFactory struct {
	baseLLMConfig config.LLMConfig
	metrics       *turnagent.PrometheusMetrics
	llmPayloadLog bool
}

// NewChatModelFactory creates a ChatModelFactory from the base infrastructure config.
func NewChatModelFactory(baseLLMConfig config.LLMConfig, metrics *turnagent.PrometheusMetrics, llmPayloadLog bool) *ChatModelFactory {
	return &ChatModelFactory{
		baseLLMConfig: baseLLMConfig,
		metrics:       metrics,
		llmPayloadLog: llmPayloadLog,
	}
}

// Create builds a new ChatModel by merging the base config with dynamic overrides.
func (f *ChatModelFactory) Create(overrides usecase.ChatModelOverrides) (model.ToolCallingChatModel, error) {
	// Clone base config and apply overrides.
	cfg := f.baseLLMConfig
	if overrides.Provider != "" {
		cfg.Provider = overrides.Provider
	}
	if overrides.Model != "" {
		cfg.Model = overrides.Model
	}
	if overrides.BaseURL != "" {
		cfg.BaseURL = overrides.BaseURL
	}
	if overrides.MaxTokens > 0 {
		cfg.MaxTokens = overrides.MaxTokens
	}
	if overrides.Temperature != nil {
		cfg.Temperature = overrides.Temperature
	}
	if overrides.ThinkingBudgetTokens > 0 {
		cfg.ThinkingBudgetTokens = overrides.ThinkingBudgetTokens
	}
	if overrides.ReasoningEffort != "" {
		cfg.ReasoningEffort = overrides.ReasoningEffort
	}
	if overrides.RetryMaxAttempts > 0 {
		cfg.RetryMaxAttempts = overrides.RetryMaxAttempts
	}

	return newChatModel(context.Background(), &cfg, f.llmPayloadLog, f.metrics)
}

// NewChatModel creates a ChatModel from config (supports Claude and OpenAI).
func NewChatModel(cfg *config.Config, metrics *turnagent.PrometheusMetrics) (model.ToolCallingChatModel, error) {
	return newChatModel(context.Background(), &cfg.LLM, cfg.Log.LLMPayload, metrics)
}

// newChatModel creates a ChatModel from config.
//
// NOTE: Only Claude protocol is fully supported and tested.
// TODO: OpenAI protocol support is incomplete. The OpenAI adapter has not been
// thoroughly tested with our message format, especially for multimodal content
// (images, files). Production deployments should only use "claude" provider.
//
// Observability transport is always injected to ensure 100% metrics coverage.
// When llmPayloadLog is enabled, full API request/response is logged to logs/llm-payload.log.
func newChatModel(ctx context.Context, cfg *config.LLMConfig, llmPayloadLog bool, metrics *turnagent.PrometheusMetrics) (model.ToolCallingChatModel, error) {
	if cfg.Provider == "" {
		return nil, fmt.Errorf("llm.provider not configured")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("llm.model not configured")
	}

	// Create metrics observer (if metrics is not nil).
	var observer LLMResponseObserver
	if metrics != nil {
		observer = NewHTTPMetricsObserver(metrics)
	}

	// Always create observability HTTP client for full metrics coverage.
	httpClient := newObservabilityHTTPClient(llmPayloadLog, observer)

	switch cfg.Provider {
	case "claude":
		return newClaudeModel(ctx, cfg, httpClient)
	case "openai":
		// WARNING: OpenAI support is experimental and not recommended for production.
		// See TODO comment in config.go for details.
		return newOpenAIModel(ctx, cfg, httpClient)
	default:
		return nil, fmt.Errorf("unsupported llm.provider: %s (supported: claude, openai)", cfg.Provider)
	}
}

// newClaudeModel creates a Claude model.
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
		AutoCacheControl: &claude.CacheControl{}, // Enable Anthropic prompt caching, reduces 60-80% cost.
	}

	// BaseURL is optional.
	if cfg.BaseURL != "" {
		claudeCfg.BaseURL = &cfg.BaseURL
	}

	// HTTPClient always injects observability transport (for metrics collection).
	claudeCfg.HTTPClient = httpClient

	chatModel, err := claude.NewChatModel(ctx, claudeCfg)
	if err != nil {
		return nil, err
	}

	// Wrap with strategic cache breakpoint support.
	// The wrapper automatically applies cache breakpoints before each API call
	// to protect stable content from microcompact/tool result budget invalidation.
	return &claudeChatModelWrapper{chatModel}, nil
}

// newOpenAIModel creates an OpenAI model.
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

	// MaxCompletionTokens corresponds to Claude's MaxTokens.
	if cfg.MaxTokens > 0 {
		openaiCfg.MaxCompletionTokens = &cfg.MaxTokens
	}

	// Temperature is optional.
	if cfg.Temperature != nil {
		openaiCfg.Temperature = cfg.Temperature
	}

	// HTTPClient always injects observability transport (for metrics collection).
	openaiCfg.HTTPClient = httpClient

	return openai.NewChatModel(ctx, openaiCfg)
}
