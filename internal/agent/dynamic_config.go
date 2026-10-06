package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
)

// resolveEffectiveValue fetches the effective config value for a key.
// Resolution order: user override > system default > yaml baseline.
// userID=nil skips user-level overrides (background calls).
func resolveEffectiveValue(ctx context.Context, configRepo repo.ConfigRepo, key string, userID *uuid.UUID) (any, error) {
	if userID != nil {
		cfg, err := configRepo.Get(ctx, key, userID)
		if err == nil {
			return unmarshalJSONBToNative(cfg.Value), nil
		}
		if !errors.Is(err, repo.ErrNotFound) {
			return nil, fmt.Errorf("get user config %q: %w", key, err)
		}
	}
	cfg, err := configRepo.Get(ctx, key, nil)
	if err == nil {
		return unmarshalJSONBToNative(cfg.Value), nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("get system config %q: %w", key, err)
	}
	if entry := config.GetRegistryEntry(key); entry != nil {
		return entry.YamlDefault, nil
	}
	return nil, nil
}

// resolveOverrides reads dynamic config values from the DB and builds ChatModelOverrides.
// Resolution order per key: user override > system default > yaml baseline.
// userID=nil skips user-level overrides (background calls).
func resolveOverrides(ctx context.Context, configRepo repo.ConfigRepo, userID *uuid.UUID) (usecase.ChatModelOverrides, error) {
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
		v, err := resolveEffectiveValue(ctx, configRepo, r.key, userID)
		if err != nil {
			return overrides, err
		}
		r.set(v)
	}

	return overrides, nil
}

// unmarshalJSONBToNative converts datatypes.JSON (raw JSON bytes) to a native Go value.
func unmarshalJSONBToNative(data []byte) any {
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
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
	if deps.ChatModelFactory == nil || deps.ConfigRepo == nil {
		return deps.ChatModel
	}

	overrides, err := resolveOverrides(ctx, deps.ConfigRepo, nil)
	if err != nil {
		return deps.ChatModel
	}

	resolved, err := deps.ChatModelFactory.Create(overrides)
	if err != nil {
		return deps.ChatModel
	}
	return resolved
}
