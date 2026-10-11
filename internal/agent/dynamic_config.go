package agent

import (
	"context"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/logger"
)

// resolveOverrides reads dynamic config values via ConfigProvider and builds ChatModelOverrides.
// Resolution order per key: user override > system default > yaml baseline.
// userID=nil skips user-level overrides (background calls).
//
// Delegates to ConfigProvider (CachedConfigProvider -> DBConfigProvider) which handles
// caching, singleflight dedup, and the three-tier resolution consistently.
func resolveOverrides(ctx context.Context, provider config.ConfigProvider, userID *uuid.UUID) (usecase.ChatModelOverrides, error) {
	overrides := usecase.ChatModelOverrides{}

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

// ResolveChatModel resolves the effective ChatModel from dynamic config for background calls.
// Exported for use by RPC handlers and other non-helper code paths.
// Falls back to deps.ChatModel when dynamic config is unavailable or resolution fails.
func ResolveChatModel(ctx context.Context, deps *usecase.Dependencies) einomodel.ToolCallingChatModel {
	if deps.ChatModelFactory == nil || deps.ConfigProvider == nil {
		return deps.ChatModel
	}

	overrides, err := resolveOverrides(ctx, deps.ConfigProvider, nil)
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
