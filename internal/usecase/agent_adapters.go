// Package usecase - agent_adapters.go
// Functions that provide handler-layer access to agent-related functionality.
// This maintains proper layer separation: Handler -> Usecase -> (internal agent details)
//
// These functions were moved from agent package to avoid circular dependencies
// and to maintain proper layering. The agent package may still contain related
// implementation details (like SessionTitleSummarizer), but the entry points
// for handler layer are exposed here.
package usecase

import (
	"context"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/config"
	appmodel "github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
)

// toInt64 converts a numeric any to int64.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// toFloat64 converts a numeric any to float64.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// ResolveOverrides reads dynamic config values via ConfigProvider and builds ChatModelOverrides.
// Resolution order per key: user override > system default > yaml baseline.
// userID=nil skips user-level overrides (background calls).
//
// Delegates to ConfigProvider (CachedConfigProvider -> DBConfigProvider) which handles
// caching, singleflight dedup, and the three-tier resolution consistently.
func ResolveOverrides(ctx context.Context, provider config.ConfigProvider, userID *uuid.UUID) (ChatModelOverrides, error) {
	overrides := ChatModelOverrides{}

	// Table-driven resolution: each entry maps a config key to a setter.
	type resolver struct {
		key string
		set func(v any)
	}
	resolvers := []resolver{
		{"llm.model", func(v any) {
			if s, ok := v.(string); ok && s != "" {
				overrides.Model = s
			}
		}},
		{"llm.provider", func(v any) {
			if s, ok := v.(string); ok && s != "" {
				overrides.Provider = s
			}
		}},
		{"llm.base_url", func(v any) {
			if s, ok := v.(string); ok {
				overrides.BaseURL = s
			}
		}},
		{"llm.max_tokens", func(v any) {
			if n, ok := toInt64(v); ok && n > 0 {
				overrides.MaxTokens = int(n)
			}
		}},
		{"llm.temperature", func(v any) {
			if f, ok := toFloat64(v); ok {
				f32 := float32(f)
				overrides.Temperature = &f32
			}
		}},
		{"llm.thinking_budget_tokens", func(v any) {
			if n, ok := toInt64(v); ok && n > 0 {
				overrides.ThinkingBudgetTokens = n
			}
		}},
		{"llm.reasoning_effort", func(v any) {
			if s, ok := v.(string); ok && s != "" {
				overrides.ReasoningEffort = s
			}
		}},
		{"llm.retry_max_attempts", func(v any) {
			if n, ok := toInt64(v); ok && n > 0 {
				overrides.RetryMaxAttempts = int(n)
			}
		}},
	}

	for _, r := range resolvers {
		// Use GetEffectiveJSON for type-agnostic resolution via ConfigProvider.
		v, err := provider.GetEffectiveJSON(ctx, r.key, userID)
		if err != nil {
			return overrides, err
		}
		r.set(v)
	}

	return overrides, nil
}

// ResolveChatModelForHandler resolves the effective ChatModel from dynamic config for background calls.
// Exported for use by RPC handlers and other non-helper code paths.
// Falls back to deps.ChatModel when dynamic config is unavailable or resolution fails.
//
// This function is placed in usecase package to maintain proper layer separation.
// Handler layer should call this instead of directly accessing agent package.
func ResolveChatModelForHandler(ctx context.Context, deps *Dependencies) einomodel.ToolCallingChatModel {
	if deps.ChatModelFactory == nil || deps.ConfigProvider == nil {
		return deps.ChatModel
	}

	overrides, err := ResolveOverrides(ctx, deps.ConfigProvider, nil)
	if err != nil {
		// Log fallback reason for observability (P1-3 fix).
		logger.Warn(ctx, "resolveChatModel.fallback",
			zap.String("reason", "resolve_overrides_failed"),
			zap.Error(err))
		return deps.ChatModel
	}

	resolved, err := deps.ChatModelFactory.Create(overrides)
	if err != nil {
		// Log fallback reason for observability (P1-4 fix).
		// Factory creation may fail due to invalid config, missing credentials, etc.
		logger.Warn(ctx, "resolveChatModel.fallback",
			zap.String("reason", "factory_create_failed"),
			zap.Any("overrides", overrides),
			zap.Error(err))
		return deps.ChatModel
	}
	return resolved
}

// FormatAskUserResultFunc is the function signature for formatting askUser results.
// This is set by the agent package during initialization to avoid circular dependencies.
// Handler layer should call FormatAskUserResultForHandler which uses this function.
var FormatAskUserResultFunc func(dbRtc *appmodel.Rtc) string

// FormatAskUserResultForHandler converts the client-submitted RTC result into the text
// that will be fed back to the LLM.
//
// This function is placed in usecase package to maintain proper layer separation.
// Handler layer should call this instead of directly accessing agent package.
// The actual implementation is provided by the agent package via FormatAskUserResultFunc.
func FormatAskUserResultForHandler(dbRtc *appmodel.Rtc) string {
	if FormatAskUserResultFunc == nil {
		// Fallback: should not happen if agent package is properly initialized
		logger.Error(context.Background(), "FormatAskUserResultFunc not initialized")
		return string(dbRtc.Result)
	}
	return FormatAskUserResultFunc(dbRtc)
}

// SessionTitleSummarizerInterface defines the interface for session title summarization.
// This allows handler layer to use the summarizer without importing agent package.
type SessionTitleSummarizerInterface interface {
	SummarizeIfNeeded(ctx context.Context, sessionID uuid.UUID) (string, error)
}

// NewSessionTitleSummarizerFunc is the function signature for creating session title summarizers.
// This is set by the agent package during initialization to avoid circular dependencies.
// Handler layer should call NewSessionTitleSummarizerForHandler which uses this function.
var NewSessionTitleSummarizerFunc func(
	chatModel einomodel.ToolCallingChatModel,
	sessionUsecase *RtcSessionUsecase,
	configProvider config.ConfigProvider,
	llmProviderFallback string,
	tokenCallbackHandler interface{},
) SessionTitleSummarizerInterface

// NewSessionTitleSummarizerForHandler creates a SessionTitleSummarizer instance.
//
// This function is placed in usecase package to maintain proper layer separation.
// Handler layer should call this instead of directly accessing agent package.
// The actual implementation is provided by the agent package via NewSessionTitleSummarizerFunc.
func NewSessionTitleSummarizerForHandler(
	chatModel einomodel.ToolCallingChatModel,
	sessionUsecase *RtcSessionUsecase,
	configProvider config.ConfigProvider,
	llmProviderFallback string,
	tokenCallbackHandler interface{},
) SessionTitleSummarizerInterface {
	if NewSessionTitleSummarizerFunc == nil {
		// Fallback: should not happen if agent package is properly initialized
		logger.Error(context.Background(), "NewSessionTitleSummarizerFunc not initialized")
		return nil
	}
	return NewSessionTitleSummarizerFunc(chatModel, sessionUsecase, configProvider, llmProviderFallback, tokenCallbackHandler)
}
