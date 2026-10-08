// Package config provides the dynamic configuration registry.
//
// The registry declares all valid dynamic configuration keys, their types,
// categories, descriptions, yaml defaults, and optional validators.
// YamlDefault values are populated at startup from the actual yaml config.
package config

import (
	"cmp"
	"fmt"
	"sort"
	"time"
)

// ValueType enumerates the supported configuration value types.
const (
	ValueTypeString   = "string"
	ValueTypeInt      = "int"
	ValueTypeFloat    = "float"
	ValueTypeBool     = "bool"
	ValueTypeDuration = "duration"
	ValueTypeJSON     = "json"
)

// Category enumerates the supported configuration categories.
const (
	CategoryLLM       = "llm"
	CategoryWorker    = "worker"
	CategoryFeature   = "feature"
	CategoryStorage   = "storage"
	CategoryAsynq     = "asynq"
	CategoryWebSearch = "web_search"
	CategoryWebFetch  = "web_fetch"
	CategoryAPI       = "api"
	CategoryLog       = "log"
)

// ConfigEntry describes a single dynamic configuration key.
type ConfigEntry struct {
	Key         string          // e.g. "llm.model"
	Category    string          // e.g. "llm"
	ValueType   string          // "string" | "int" | "float" | "bool" | "duration" | "json"
	Description string          // human-readable description
	YamlDefault any             // yaml config baseline value (populated at startup)
	Validator   func(any) error // optional value validator; receives parsed Go value
}

// DynamicConfigRegistry is the authoritative map of all dynamic configuration keys.
// Keys must match [a-zA-Z0-9_.] and use dot-separated namespaces.
var DynamicConfigRegistry = map[string]*ConfigEntry{
	// ── LLM ──────────────────────────────────────────────────────────
	"llm.model": {
		Key:         "llm.model",
		Category:    CategoryLLM,
		ValueType:   ValueTypeString,
		Description: "LLM model name",
		YamlDefault: "claude-sonnet-4-5",
	},
	"llm.provider": {
		Key:         "llm.provider",
		Category:    CategoryLLM,
		ValueType:   ValueTypeString,
		Description: "LLM provider (claude / openai)",
		YamlDefault: "claude",
	},
	"llm.base_url": {
		Key:         "llm.base_url",
		Category:    CategoryLLM,
		ValueType:   ValueTypeString,
		Description: "LLM API base URL (proxy / enterprise endpoint)",
		YamlDefault: "",
	},
	"llm.max_tokens": {
		Key:         "llm.max_tokens",
		Category:    CategoryLLM,
		ValueType:   ValueTypeInt,
		Description: "Maximum output tokens",
		YamlDefault: 16000,
		Validator: func(v any) error {
			if n, ok := v.(int); ok && n <= 0 {
				return fmt.Errorf("max_tokens must be > 0, got %d", n)
			}
			return nil
		},
	},
	"llm.temperature": {
		Key:         "llm.temperature",
		Category:    CategoryLLM,
		ValueType:   ValueTypeFloat,
		Description: "Sampling temperature (0.0-2.0)",
		YamlDefault: 1.0,
	},
	"llm.thinking_budget_tokens": {
		Key:         "llm.thinking_budget_tokens",
		Category:    CategoryLLM,
		ValueType:   ValueTypeInt,
		Description: "Claude thinking mode token budget",
		YamlDefault: 10000,
		Validator: func(v any) error {
			if n, ok := v.(int); ok && n < 0 {
				return fmt.Errorf("thinking_budget_tokens must be >= 0, got %d", n)
			}
			return nil
		},
	},
	"llm.reasoning_effort": {
		Key:         "llm.reasoning_effort",
		Category:    CategoryLLM,
		ValueType:   ValueTypeString,
		Description: "OpenAI reasoning effort level (low / medium / high)",
		YamlDefault: "medium",
	},
	"llm.retry_max_attempts": {
		Key:         "llm.retry_max_attempts",
		Category:    CategoryLLM,
		ValueType:   ValueTypeInt,
		Description: "Maximum retry count on LLM call failure (0 = no retry)",
		YamlDefault: 0,
		Validator: func(v any) error {
			if n, ok := v.(int); ok && n < 0 {
				return fmt.Errorf("retry_max_attempts must be >= 0, got %d", n)
			}
			return nil
		},
	},
	"llm.retry_base_delay": {
		Key:         "llm.retry_base_delay",
		Category:    CategoryLLM,
		ValueType:   ValueTypeDuration,
		Description: "Base backoff duration for retries (Go duration string)",
		YamlDefault: "1s",
		Validator:   ValidateDuration,
	},
	"llm.pricing.input_per_million": {
		Key:         "llm.pricing.input_per_million",
		Category:    CategoryLLM,
		ValueType:   ValueTypeFloat,
		Description: "Input token price (USD per million tokens)",
		YamlDefault: 3.0,
	},
	"llm.pricing.output_per_million": {
		Key:         "llm.pricing.output_per_million",
		Category:    CategoryLLM,
		ValueType:   ValueTypeFloat,
		Description: "Output token price (USD per million tokens)",
		YamlDefault: 15.0,
	},
	"llm.pricing.cached_read_per_million": {
		Key:         "llm.pricing.cached_read_per_million",
		Category:    CategoryLLM,
		ValueType:   ValueTypeFloat,
		Description: "Cache read token price (USD per million tokens)",
		YamlDefault: 0.3,
	},
	"llm.pricing.cached_write_per_million": {
		Key:         "llm.pricing.cached_write_per_million",
		Category:    CategoryLLM,
		ValueType:   ValueTypeFloat,
		Description: "Cache write token price (USD per million tokens)",
		YamlDefault: 3.75,
	},
	"llm.pricing.reasoning_per_million": {
		Key:         "llm.pricing.reasoning_per_million",
		Category:    CategoryLLM,
		ValueType:   ValueTypeFloat,
		Description: "Reasoning token price (USD per million tokens)",
		YamlDefault: 0.0,
	},

	// ── Worker / Agent Behavior ──────────────────────────────────────
	"worker.system_prompt": {
		Key:         "worker.system_prompt",
		Category:    CategoryWorker,
		ValueType:   ValueTypeString,
		Description: "Agent system prompt",
		YamlDefault: "",
	},
	"worker.context_tokens_limit": {
		Key:         "worker.context_tokens_limit",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Token threshold for triggering context compression",
		YamlDefault: 25000,
		Validator: func(v any) error {
			if n, ok := v.(int); ok && n <= 0 {
				return fmt.Errorf("context_tokens_limit must be > 0, got %d", n)
			}
			return nil
		},
	},
	"worker.auto_compact_buffer_tokens": {
		Key:         "worker.auto_compact_buffer_tokens",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Auto-compact buffer token count",
		YamlDefault: 13000,
	},
	"worker.max_output_tokens_for_summary": {
		Key:         "worker.max_output_tokens_for_summary",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Maximum output tokens for compression summary",
		YamlDefault: 20000,
	},
	"worker.background_concurrency": {
		Key:         "worker.background_concurrency",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Background task concurrency",
		YamlDefault: 5,
	},
	"worker.token_counter_mode": {
		Key:         "worker.token_counter_mode",
		Category:    CategoryWorker,
		ValueType:   ValueTypeString,
		Description: "Token counting strategy (heuristic / tokenizer)",
		YamlDefault: "heuristic",
	},
	"worker.enable_strategic_cache_breakpoints": {
		Key:         "worker.enable_strategic_cache_breakpoints",
		Category:    CategoryWorker,
		ValueType:   ValueTypeBool,
		Description: "Enable strategic cache breakpoints",
		YamlDefault: true,
	},
	"worker.cache_hit_rate_warn_threshold": {
		Key:         "worker.cache_hit_rate_warn_threshold",
		Category:    CategoryWorker,
		ValueType:   ValueTypeFloat,
		Description: "Cache hit rate warning threshold (0.0-1.0)",
		YamlDefault: 0.88,
	},
	"worker.idle_timeout": {
		Key:         "worker.idle_timeout",
		Category:    CategoryWorker,
		ValueType:   ValueTypeDuration,
		Description: "Worker idle timeout (Go duration string)",
		YamlDefault: "5m",
		Validator:   ValidateDuration,
	},
	"worker.heartbeat_sec": {
		Key:         "worker.heartbeat_sec",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Worker heartbeat interval in seconds",
		YamlDefault: 10,
	},
	"worker.ttl_sec": {
		Key:         "worker.ttl_sec",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Worker TTL in seconds",
		YamlDefault: 60,
	},
	"worker.lock_ttl_sec": {
		Key:         "worker.lock_ttl_sec",
		Category:    CategoryWorker,
		ValueType:   ValueTypeInt,
		Description: "Session lock TTL in seconds",
		YamlDefault: 120,
	},
	"worker.checkpoint_ttl": {
		Key:         "worker.checkpoint_ttl",
		Category:    CategoryWorker,
		ValueType:   ValueTypeDuration,
		Description: "Eino checkpoint TTL (Go duration string)",
		YamlDefault: "24h",
		Validator:   ValidateDuration,
	},
	"worker.stream_chunk_ttl": {
		Key:         "worker.stream_chunk_ttl",
		Category:    CategoryWorker,
		ValueType:   ValueTypeDuration,
		Description: "Streaming message chunk TTL (Go duration string)",
		YamlDefault: "5m",
		Validator:   ValidateDuration,
	},
	"worker.interrupt_answer_ttl": {
		Key:         "worker.interrupt_answer_ttl",
		Category:    CategoryWorker,
		ValueType:   ValueTypeDuration,
		Description: "Interrupt answer TTL (Go duration string)",
		YamlDefault: "10m",
		Validator:   ValidateDuration,
	},
	"worker.orphan_trigger_ttl": {
		Key:         "worker.orphan_trigger_ttl",
		Category:    CategoryWorker,
		ValueType:   ValueTypeDuration,
		Description: "Orphan trigger dedup TTL (Go duration string)",
		YamlDefault: "24h",
		Validator:   ValidateDuration,
	},

	// ── Feature Flags ────────────────────────────────────────────────
	"feature.web_search": {
		Key:         "feature.web_search",
		Category:    CategoryFeature,
		ValueType:   ValueTypeBool,
		Description: "Web search feature toggle",
		YamlDefault: false,
	},
	"feature.web_fetch": {
		Key:         "feature.web_fetch",
		Category:    CategoryFeature,
		ValueType:   ValueTypeBool,
		Description: "Web fetch feature toggle",
		YamlDefault: false,
	},
	"feature.debug": {
		Key:         "feature.debug",
		Category:    CategoryFeature,
		ValueType:   ValueTypeBool,
		Description: "Debug endpoints toggle",
		YamlDefault: false,
	},
	"feature.debug_show_raw_errors": {
		Key:         "feature.debug_show_raw_errors",
		Category:    CategoryFeature,
		ValueType:   ValueTypeBool,
		Description: "Show raw errors in debug endpoints",
		YamlDefault: false,
	},
	"feature.log_llm_payload": {
		Key:         "feature.log_llm_payload",
		Category:    CategoryFeature,
		ValueType:   ValueTypeBool,
		Description: "Enable full LLM payload logging",
		YamlDefault: false,
	},

	// ── Storage ──────────────────────────────────────────────────────
	"storage.quota.max_file_size_bytes": {
		Key:         "storage.quota.max_file_size_bytes",
		Category:    CategoryStorage,
		ValueType:   ValueTypeInt,
		Description: "Maximum single file size in bytes",
		YamlDefault: 104857600, // 100MB
	},
	"storage.quota.max_user_quota_bytes": {
		Key:         "storage.quota.max_user_quota_bytes",
		Category:    CategoryStorage,
		ValueType:   ValueTypeInt,
		Description: "Maximum user storage quota in bytes",
		YamlDefault: 1073741824, // 1GB
	},
	"storage.rate_limit.requests_per_minute": {
		Key:         "storage.rate_limit.requests_per_minute",
		Category:    CategoryStorage,
		ValueType:   ValueTypeInt,
		Description: "S3 API rate limit (requests per minute)",
		YamlDefault: 60,
	},
	"storage.cleanup.interval": {
		Key:         "storage.cleanup.interval",
		Category:    CategoryStorage,
		ValueType:   ValueTypeDuration,
		Description: "Cleanup task interval (Go duration string)",
		YamlDefault: "1h",
		Validator:   ValidateDuration,
	},
	"storage.cleanup.multipart_expiry": {
		Key:         "storage.cleanup.multipart_expiry",
		Category:    CategoryStorage,
		ValueType:   ValueTypeDuration,
		Description: "Incomplete multipart upload expiry (Go duration string)",
		YamlDefault: "24h",
		Validator:   ValidateDuration,
	},
	"storage.cleanup.orphan.cooldown_period": {
		Key:         "storage.cleanup.orphan.cooldown_period",
		Category:    CategoryStorage,
		ValueType:   ValueTypeDuration,
		Description: "Orphan object cooldown period (Go duration string)",
		YamlDefault: "1h",
		Validator:   ValidateDuration,
	},

	// ── Web Search ───────────────────────────────────────────────────
	"web_search.rate_limiter.rate": {
		Key:         "web_search.rate_limiter.rate",
		Category:    CategoryWebSearch,
		ValueType:   ValueTypeFloat,
		Description: "Web search rate limit (requests per second)",
		YamlDefault: 10.0,
	},
	"web_search.rate_limiter.burst": {
		Key:         "web_search.rate_limiter.burst",
		Category:    CategoryWebSearch,
		ValueType:   ValueTypeInt,
		Description: "Web search burst limit",
		YamlDefault: 20,
	},

	// ── Web Fetch ────────────────────────────────────────────────────
	"web_fetch.max_concurrency": {
		Key:         "web_fetch.max_concurrency",
		Category:    CategoryWebFetch,
		ValueType:   ValueTypeInt,
		Description: "Global max concurrent fetches",
		YamlDefault: 20,
	},
	"web_fetch.max_domain_concurrency": {
		Key:         "web_fetch.max_domain_concurrency",
		Category:    CategoryWebFetch,
		ValueType:   ValueTypeInt,
		Description: "Per-domain max concurrent fetches",
		YamlDefault: 5,
	},
	"web_fetch.blocked_domains": {
		Key:         "web_fetch.blocked_domains",
		Category:    CategoryWebFetch,
		ValueType:   ValueTypeJSON,
		Description: "Blocked domains list (JSON array)",
		YamlDefault: []any{},
	},
	"web_fetch.pre_approved_domains": {
		Key:         "web_fetch.pre_approved_domains",
		Category:    CategoryWebFetch,
		ValueType:   ValueTypeJSON,
		Description: "Pre-approved domains list (JSON array)",
		YamlDefault: []any{},
	},

	// ── API ──────────────────────────────────────────────────────────
	"api.query_default_limit": {
		Key:         "api.query_default_limit",
		Category:    CategoryAPI,
		ValueType:   ValueTypeInt,
		Description: "Default page size for paginated queries",
		YamlDefault: 50,
	},
	"api.query_max_limit": {
		Key:         "api.query_max_limit",
		Category:    CategoryAPI,
		ValueType:   ValueTypeInt,
		Description: "Maximum page size for paginated queries",
		YamlDefault: 100,
	},

	// ── Log ──────────────────────────────────────────────────────────
	"log.level": {
		Key:         "log.level",
		Category:    CategoryLog,
		ValueType:   ValueTypeString,
		Description: "Log level (debug / info / warn / error)",
		YamlDefault: "info",
	},

	// ── Asynq ────────────────────────────────────────────────────────
	"asynq.concurrency": {
		Key:         "asynq.concurrency",
		Category:    CategoryAsynq,
		ValueType:   ValueTypeInt,
		Description: "Background task concurrency",
		YamlDefault: 10,
	},
	"asynq.retry_max": {
		Key:         "asynq.retry_max",
		Category:    CategoryAsynq,
		ValueType:   ValueTypeInt,
		Description: "Task maximum retry count",
		YamlDefault: 3,
	},
	"asynq.retry_timeout": {
		Key:         "asynq.retry_timeout",
		Category:    CategoryAsynq,
		ValueType:   ValueTypeDuration,
		Description: "Task execution timeout (Go duration string)",
		YamlDefault: "30s",
		Validator:   ValidateDuration,
	},
	"asynq.stale_threshold": {
		Key:         "asynq.stale_threshold",
		Category:    CategoryAsynq,
		ValueType:   ValueTypeDuration,
		Description: "Loop staleness threshold (Go duration string)",
		YamlDefault: "5m",
		Validator:   ValidateDuration,
	},
	"asynq.health_check_interval": {
		Key:         "asynq.health_check_interval",
		Category:    CategoryAsynq,
		ValueType:   ValueTypeDuration,
		Description: "Health check interval (Go duration string)",
		YamlDefault: "30s",
		Validator:   ValidateDuration,
	},
}

// ValidateDuration checks that a string is a valid positive Go duration.
func ValidateDuration(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("duration value must be a string, got %T", v)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d <= 0 {
		return fmt.Errorf("duration must be positive, got %s", d)
	}
	return nil
}

// GetRegistryEntry returns the registry entry for a key, or nil if not found.
func GetRegistryEntry(key string) *ConfigEntry {
	return DynamicConfigRegistry[key]
}

// AllRegistryKeys returns all registered keys sorted by category then key.
func AllRegistryKeys() []string {
	keys := make([]string, 0, len(DynamicConfigRegistry))
	for k := range DynamicConfigRegistry {
		keys = append(keys, k)
	}
	// Sort by category, then key (stable ordering for API pagination).
	sortConfigKeys(keys)
	return keys
}

// sortConfigKeys sorts keys by category prefix, then alphabetically.
// Uses sort.SliceStable for clarity; registry is small (~50 keys), called once per request.
func sortConfigKeys(keys []string) {
	sort.SliceStable(keys, func(i, j int) bool {
		return compareConfigKeys(keys[i], keys[j]) < 0
	})
}

func compareConfigKeys(a, b string) int {
	catA, catB := categoryOf(a), categoryOf(b)
	if c := cmp.Compare(catA, catB); c != 0 {
		return c
	}
	return cmp.Compare(a, b)
}

func categoryOf(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i]
		}
	}
	return key
}

// PopulateYamlDefaults fills registry YamlDefault from the loaded Config.
// Called once at startup after config loading.
func PopulateYamlDefaults(cfg *Config) {
	setDefault := func(key string, val any) {
		if entry, ok := DynamicConfigRegistry[key]; ok {
			entry.YamlDefault = val
		}
	}

	// LLM
	setDefault("llm.model", cfg.LLM.Model)
	setDefault("llm.provider", cfg.LLM.Provider)
	setDefault("llm.base_url", cfg.LLM.BaseURL)
	setDefault("llm.max_tokens", cfg.LLM.MaxTokens)
	if cfg.LLM.Temperature != nil {
		setDefault("llm.temperature", float64(*cfg.LLM.Temperature))
	}
	setDefault("llm.thinking_budget_tokens", int(cfg.LLM.ThinkingBudgetTokens))
	setDefault("llm.reasoning_effort", cfg.LLM.ReasoningEffort)
	setDefault("llm.retry_max_attempts", cfg.LLM.RetryMaxAttempts)
	setDefault("llm.retry_base_delay", cfg.LLM.RetryBaseDelay.String())
	if cfg.LLM.Pricing != nil {
		setDefault("llm.pricing.input_per_million", cfg.LLM.Pricing.InputPerMillion)
		setDefault("llm.pricing.output_per_million", cfg.LLM.Pricing.OutputPerMillion)
		setDefault("llm.pricing.cached_read_per_million", cfg.LLM.Pricing.CachedReadPerMillion)
		setDefault("llm.pricing.cached_write_per_million", cfg.LLM.Pricing.CachedWritePerMillion)
		setDefault("llm.pricing.reasoning_per_million", cfg.LLM.Pricing.ReasoningPerMillion)
	}

	// Worker
	setDefault("worker.system_prompt", cfg.Worker.SystemPrompt)
	setDefault("worker.context_tokens_limit", cfg.Worker.ContextTokensLimit)
	setDefault("worker.auto_compact_buffer_tokens", cfg.Worker.AutoCompactBufferTokens)
	setDefault("worker.max_output_tokens_for_summary", cfg.Worker.MaxOutputTokensForSummary)
	setDefault("worker.background_concurrency", cfg.Worker.BackgroundConcurrency)
	setDefault("worker.token_counter_mode", cfg.Worker.TokenCounterMode)
	setDefault("worker.enable_strategic_cache_breakpoints", cfg.Worker.EnableStrategicCacheBreakpoints)
	setDefault("worker.cache_hit_rate_warn_threshold", cfg.Worker.CacheHitRateWarnThreshold)
	setDefault("worker.idle_timeout", cfg.Worker.IdleTimeout.String())
	setDefault("worker.heartbeat_sec", cfg.Worker.HeartbeatSec)
	setDefault("worker.ttl_sec", cfg.Worker.TTLSec)
	setDefault("worker.lock_ttl_sec", cfg.Worker.LockTTLSeconds)
	setDefault("worker.checkpoint_ttl", cfg.Worker.CheckpointTTL.String())
	setDefault("worker.stream_chunk_ttl", cfg.Worker.StreamChunkTTL.String())
	setDefault("worker.interrupt_answer_ttl", cfg.Worker.InterruptAnswerTTL.String())
	setDefault("worker.orphan_trigger_ttl", cfg.Worker.OrphanTriggerTTL.String())

	// Feature flags
	setDefault("feature.web_search", cfg.WebSearch.Enabled)
	setDefault("feature.web_fetch", cfg.WebFetch.Enabled)
	setDefault("feature.debug", cfg.Debug.Enabled)
	setDefault("feature.debug_show_raw_errors", cfg.Debug.ShowRawErrors)
	setDefault("feature.log_llm_payload", cfg.Log.LLMPayload)

	// Storage
	setDefault("storage.quota.max_file_size_bytes", int(cfg.Storage.Quota.MaxFileSizeBytes))
	setDefault("storage.quota.max_user_quota_bytes", int(cfg.Storage.Quota.MaxUserQuotaBytes))
	setDefault("storage.rate_limit.requests_per_minute", cfg.Storage.RateLimit.RequestsPerMinute)
	setDefault("storage.cleanup.interval", cfg.Storage.Cleanup.Interval.String())
	setDefault("storage.cleanup.multipart_expiry", cfg.Storage.Cleanup.MultipartExpiry.String())
	setDefault("storage.cleanup.orphan.cooldown_period", cfg.Storage.Cleanup.Orphan.CooldownPeriod.String())

	// Web Search
	setDefault("web_search.rate_limiter.rate", cfg.WebSearch.RateLimiter.Rate)
	setDefault("web_search.rate_limiter.burst", cfg.WebSearch.RateLimiter.Burst)

	// Web Fetch
	setDefault("web_fetch.max_concurrency", cfg.WebFetch.MaxConcurrency)
	setDefault("web_fetch.max_domain_concurrency", cfg.WebFetch.MaxDomainConcurrency)
	if len(cfg.WebFetch.BlockedDomains) > 0 {
		ifaces := make([]any, len(cfg.WebFetch.BlockedDomains))
		for i, d := range cfg.WebFetch.BlockedDomains {
			ifaces[i] = d
		}
		setDefault("web_fetch.blocked_domains", ifaces)
	}
	if len(cfg.WebFetch.PreApprovedDomains) > 0 {
		ifaces := make([]any, len(cfg.WebFetch.PreApprovedDomains))
		for i, d := range cfg.WebFetch.PreApprovedDomains {
			ifaces[i] = d
		}
		setDefault("web_fetch.pre_approved_domains", ifaces)
	}

	// API
	setDefault("api.query_default_limit", cfg.API.QueryDefaultLimit)
	setDefault("api.query_max_limit", cfg.API.QueryMaxLimit)

	// Log
	setDefault("log.level", cfg.Log.Level)

	// Asynq
	setDefault("asynq.concurrency", cfg.Asynq.Concurrency)
	setDefault("asynq.retry_max", cfg.Asynq.RetryMax)
	setDefault("asynq.retry_timeout", cfg.Asynq.RetryTimeout.String())
	setDefault("asynq.stale_threshold", cfg.Asynq.StaleThreshold.String())
	setDefault("asynq.health_check_interval", cfg.Asynq.HealthCheckInterval.String())
}
