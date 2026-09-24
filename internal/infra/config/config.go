package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the top-level application configuration, aggregating all sub-module configs.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Log       LogConfig       `mapstructure:"log"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Providers ProvidersConfig `mapstructure:"providers"`
	CORS      CORSConfig      `mapstructure:"cors"`
	Worker    WorkerConfig    `mapstructure:"worker"`
	LLM       LLMConfig       `mapstructure:"llm"`
	API       APIConfig       `mapstructure:"api"`
	Tracing   TracingConfig   `mapstructure:"tracing"`
	Asynq     AsynqConfig     `mapstructure:"asynq"`
	Metrics   MetricsConfig   `mapstructure:"metrics"`
	Debug     DebugConfig     `mapstructure:"debug"`
}

// MetricsConfig holds Prometheus /metrics endpoint authentication configuration.
type MetricsConfig struct {
	// User is the basic auth username. Empty disables authentication (development only).
	User string `mapstructure:"user"`
	// Password is the basic auth password.
	Password string `mapstructure:"password"`
}

// DebugConfig holds /debug/pprof and /debug/goroutines endpoint configuration.
// These endpoints expose internal process state; authentication must be configured in production.
type DebugConfig struct {
	// Enabled controls whether debug endpoints are active. Defaults to true.
	// Set to false to completely disable /debug/* routes.
	Enabled bool `mapstructure:"enabled"`
	// User is the basic auth username. Empty disables authentication (development only).
	User string `mapstructure:"user"`
	// Password is the basic auth password.
	Password string `mapstructure:"password"`
	// GoroutineLeakThreshold logs a warning when goroutine count exceeds this value.
	// Default 1000. Set to 0 to disable the alert.
	GoroutineLeakThreshold int `mapstructure:"goroutine_leak_threshold"`
	// ShowRawErrors controls whether RawError content is shown in error messages.
	// Independent of Enabled; defaults to false. Even when debug endpoints are active (for pprof debugging),
	// RawError is not automatically exposed to end users — must be explicitly enabled.
	// Production deployments must keep this false or explicitly set it to false.
	ShowRawErrors bool `mapstructure:"show_raw_errors"`
}

// AsynqConfig holds asynq task scheduling configuration.
type AsynqConfig struct {
	// RedisAddr is the Redis address. Defaults to the main Redis.
	RedisAddr string `mapstructure:"redis_addr"`
	// RedisPassword is the Redis password.
	RedisPassword string `mapstructure:"redis_password"`
	// RedisDB is the Redis DB number.
	RedisDB int `mapstructure:"redis_db"`
	// Concurrency is the asynq worker concurrency.
	Concurrency int `mapstructure:"concurrency"`
	// Queue is the queue name.
	Queue string `mapstructure:"queue"`
	// RecoveryInterval is the recovery scan interval.
	RecoveryInterval time.Duration `mapstructure:"recovery_interval"`

	// StaleThreshold is the time threshold for determining a loop is stale.
	// An active loop that has not produced an asynq task within this period is considered stale and re-enqueued.
	// Default 5 minutes.
	StaleThreshold time.Duration `mapstructure:"stale_threshold"`

	// RetryMax is the maximum number of retries on task failure.
	// Default 3.
	RetryMax int `mapstructure:"retry_max"`

	// RetryTimeout is the task execution timeout.
	// Default 30 seconds.
	RetryTimeout time.Duration `mapstructure:"retry_timeout"`

	// HealthCheckInterval is the health check interval.
	// Default 30 seconds.
	HealthCheckInterval time.Duration `mapstructure:"health_check_interval"`
}

// ServerConfig holds HTTP/WebSocket server listen address configuration.
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`

	// Env is the runtime environment: development / production.
	// Defaults to production; development allows relaxed fallbacks (e.g., WebSocket accepts any Origin).
	Env string `mapstructure:"env"`

	// ShutdownTimeout is the graceful shutdown timeout (waiting for in-flight requests to complete).
	// Default 10s.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`

	// RPCTimeout is the RPC handler context timeout.
	// Default 10s.
	RPCTimeout time.Duration `mapstructure:"rpc_timeout"`
}

// DatabaseConfig holds database connection and migration configuration.
type DatabaseConfig struct {
	DSN         string `mapstructure:"dsn"`
	AutoMigrate bool   `mapstructure:"auto_migrate"`
}

// LogConfig holds log level configuration.
type LogConfig struct {
	Level string `mapstructure:"level"`

	// LLMPayload enables writing the full LLM API request/response as JSON Lines
	// to logs/llm-payload.log.
	// Only for development/debugging; do not enable in production.
	LLMPayload bool `mapstructure:"llm_payload"`

	// ServerLogFile is the server log file path (JSON format).
	// When set, all logs are written to both stdout and this file.
	// Used for promtail host log collection in development. Leave empty to skip file output.
	ServerLogFile string `mapstructure:"server_log_file"`
}

// RedisConfig holds Redis connection configuration.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// AuthConfig holds JWT / token configuration.
type AuthConfig struct {
	JWTSecret             string        `mapstructure:"jwt_secret"`
	AccessTokenTTLSeconds int           `mapstructure:"access_token_ttl_seconds"`
	RefreshTokenTTL       time.Duration `mapstructure:"refresh_token_ttl"` // Default 30 days.
	OAuth2StateTTL        time.Duration `mapstructure:"oauth2_state_ttl"`  // Default 10 minutes.
	// AllowedRedirectURIs is the OAuth2 redirect_uri whitelist.
	// Empty means no restriction (development only); production must configure explicitly.
	AllowedRedirectURIs []string `mapstructure:"allowed_redirect_uris"`
}

// ProvidersConfig holds the OAuth2 provider configuration set.
type ProvidersConfig struct {
	Mock        MockProviderConfig   `mapstructure:"mock"`
	GitHub      GitHubProviderConfig `mapstructure:"github"`
	Google      GoogleProviderConfig `mapstructure:"google"`
	HTTPTimeout time.Duration        `mapstructure:"http_timeout"` // OAuth2 HTTP client timeout, default 10s.
}

// MockProviderConfig holds Mock OAuth2 provider configuration.
type MockProviderConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	URL          string `mapstructure:"url"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
}

// GitHubProviderConfig holds GitHub OAuth2 provider configuration.
type GitHubProviderConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
	// Scope is the requested permission scope. Default "read:user user:email".
	Scope string `mapstructure:"scope"`
}

// GoogleProviderConfig holds Google OAuth2 provider configuration.
type GoogleProviderConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
	// Scope is the requested permission scope. Default "openid email profile".
	Scope string `mapstructure:"scope"`
}

// CORSConfig holds cross-origin resource sharing configuration.
type CORSConfig struct {
	// AllowOrigins is the list of allowed origins, e.g., ["https://app.example.com"].
	// Empty falls back to "*" (development only; production must configure explicitly).
	AllowOrigins []string `mapstructure:"allow_origins"`
}

// WorkerConfig holds Worker lifecycle and TurnLoop configuration.
type WorkerConfig struct {
	WorkerID              string        `mapstructure:"worker_id"`
	Host                  string        `mapstructure:"host"`
	Version               string        `mapstructure:"version"`
	HeartbeatSec          int           `mapstructure:"heartbeat_sec"`          // Default 10.
	TTLSec                int           `mapstructure:"ttl_sec"`                // Default 60.
	IdleTimeout           time.Duration `mapstructure:"idle_timeout"`           // Default 5m.
	StreamBlock           time.Duration `mapstructure:"stream_block"`           // Default 500ms.
	MaxLenApprox          int64         `mapstructure:"max_len_approx"`         // Default 10000.
	BackgroundConcurrency int           `mapstructure:"background_concurrency"` // Default 5.
	SystemPrompt          string        `mapstructure:"system_prompt"`          // Agent system prompt.

	// ContextTokensLimit is the token threshold for triggering context compression (optional).
	// Default 25000 (approximately 20% of the model context window).
	ContextTokensLimit int `mapstructure:"context_tokens_limit"`

	// AutoCompactBufferTokens is the auto-compact buffer token count (optional).
	// Used to calculate the actual trigger threshold: ContextTokensLimit - AutoCompactBufferTokens.
	// Default 13000.
	AutoCompactBufferTokens int `mapstructure:"auto_compact_buffer_tokens"`

	// MaxOutputTokensForSummary is the maximum output tokens for compression (optional).
	// Default 20000.
	MaxOutputTokensForSummary int `mapstructure:"max_output_tokens_for_summary"`

	// CheckpointTTL is the eino checkpoint TTL in Redis.
	// Default 24h. A longer TTL improves crash recovery window but increases Redis memory pressure.
	CheckpointTTL time.Duration `mapstructure:"checkpoint_ttl"`

	// StreamChunkTTL is the streaming message chunk TTL in Redis.
	// Default 5m. Chunks are short-lived data, deleted after generation completes.
	// Note: longer thinking/reasoning streams may require a longer TTL to avoid chunk loss.
	StreamChunkTTL time.Duration `mapstructure:"stream_chunk_ttl"`

	// InterruptAnswerTTL is the interrupt answer TTL in Redis.
	// Default 10m.
	InterruptAnswerTTL time.Duration `mapstructure:"interrupt_answer_ttl"`

	// OrphanTriggerTTL is the RTC orphan recovery dedup marker TTL.
	// Default 24h.
	OrphanTriggerTTL time.Duration `mapstructure:"orphan_trigger_ttl"`

	// LockTTLSeconds is the rtc-queue session lock TTL in seconds.
	// Default 120.
	LockTTLSeconds int `mapstructure:"lock_ttl_sec"`

	// CacheHitRateWarnThreshold is the cache hit rate alert threshold (optional).
	// Session cumulative cache hit rate = TotalCachedReadTokens / (TotalCachedReadTokens + TotalInputTokens).
	// A warning is logged when the rate falls below this threshold. Negative values disable the alert.
	// Default 0.88 (88%).
	CacheHitRateWarnThreshold float64 `mapstructure:"cache_hit_rate_warn_threshold"`

	// TokenCounterMode controls the token counting strategy (optional).
	// Options:
	//   - "heuristic" (default): fast, ~4 chars/token, suitable for English text.
	//   - "tokenizer": precise, uses cl100k_base encoding (tiktoken-go), 3-4x better accuracy for CJK.
	// Note: tokenizer mode takes ~50-200ms to load initially, ~5MB memory.
	TokenCounterMode string `mapstructure:"token_counter_mode"`

	// EnableStrategicCacheBreakpoints enables strategic cache breakpoints (optional).
	// By setting cache breakpoints at key positions, protects stable content from cache invalidation
	// caused by microcompact or tool result budget modifications.
	// When enabled, cache hit rate after microcompact improves from 0% to 75%, input cost reduced by ~69%.
	// Default true.
	EnableStrategicCacheBreakpoints bool `mapstructure:"enable_strategic_cache_breakpoints"`
}

// LLMConfig holds LLM model configuration (supports Claude and OpenAI protocols).
type LLMConfig struct {
	// Provider is the model provider: "claude" or "openai".
	Provider string `mapstructure:"provider"`

	// APIKey is the API key.
	APIKey string `mapstructure:"api_key"`

	// BaseURL is a custom API endpoint (optional, for proxies or enterprise deployments).
	BaseURL string `mapstructure:"base_url"`

	// Model is the model name (e.g., "claude-3-5-sonnet-20241022", "gpt-4o").
	Model string `mapstructure:"model"`

	// MaxTokens is the maximum output tokens (required for Claude, optional for OpenAI).
	MaxTokens int `mapstructure:"max_tokens"`

	// Temperature is the sampling temperature (optional for OpenAI, 0.0-2.0).
	Temperature *float32 `mapstructure:"temperature"`

	// Timeout is the API request timeout (optional).
	Timeout time.Duration `mapstructure:"timeout"`

	// ThinkingBudgetTokens is the Claude thinking mode token budget (optional).
	// Only effective for Claude models that support thinking. Default 50000.
	ThinkingBudgetTokens int64 `mapstructure:"thinking_budget_tokens"`

	// ReasoningEffort is the OpenAI reasoning model effort level (optional).
	// Options: "low", "medium", "high". Default "medium".
	ReasoningEffort string `mapstructure:"reasoning_effort"`

	// RetryMaxAttempts is the maximum retry count on model call failure (optional).
	// Default 0 (no retry). Recommended to set to 3 in production.
	RetryMaxAttempts int `mapstructure:"retry_max_attempts"`

	// RetryBaseDelay is the base backoff duration for retries (optional).
	// Default 1s. Actual backoff = RetryBaseDelay * 2^(attempt-1), i.e., exponential backoff.
	RetryBaseDelay time.Duration `mapstructure:"retry_base_delay"`

	// Pricing is the model pricing configuration (optional, for cost calculation).
	// Uses default pricing (Claude 3.5 Sonnet) when not configured.
	Pricing *ModelPricingConfig `mapstructure:"pricing"`
}

// ModelPricingConfig holds model pricing configuration (USD per million tokens).
type ModelPricingConfig struct {
	// InputPerMillion is the normal input token price (USD).
	InputPerMillion float64 `mapstructure:"input_per_million"`

	// OutputPerMillion is the output token price (USD).
	OutputPerMillion float64 `mapstructure:"output_per_million"`

	// CachedReadPerMillion is the cache read (cache hit) price (USD).
	// Typically 10% of input price.
	CachedReadPerMillion float64 `mapstructure:"cached_read_per_million"`

	// CachedWritePerMillion is the cache write (cache creation) price (USD).
	// Typically 125% of input price.
	CachedWritePerMillion float64 `mapstructure:"cached_write_per_million"`

	// ReasoningPerMillion is the reasoning (thinking) token price (USD).
	ReasoningPerMillion float64 `mapstructure:"reasoning_per_million"`
}

// APIConfig holds API-layer configuration (pagination, rate limiting, etc.).
type APIConfig struct {
	// QueryDefaultLimit is the default page size for paginated queries. Default 50.
	QueryDefaultLimit int `mapstructure:"query_default_limit"`

	// QueryMaxLimit is the maximum page size for paginated queries. Default 100.
	QueryMaxLimit int `mapstructure:"query_max_limit"`
}

// TracingConfig holds OpenTelemetry distributed tracing configuration.
type TracingConfig struct {
	// Enabled controls whether tracing is active.
	Enabled bool `mapstructure:"enabled"`

	// Endpoint is the OTLP endpoint (e.g., "localhost:4317").
	Endpoint string `mapstructure:"endpoint"`

	// SampleRate is the sampling rate (0.0 - 1.0). Default 1.0 (full sampling).
	SampleRate float64 `mapstructure:"sample_rate"`
}

// Load loads configuration. Uses a local viper instance to avoid polluting global state; safe for parallel tests.
//
// Config merge strategy (when --config is not specified):
//  1. Load etc/config.yaml as baseline
//  2. If etc/config.local.yaml exists, merge and override baseline (only write differences)
//  3. config.local.yaml should be in .gitignore, used for local personal configuration
//
// When --config is specified, only loads the specified file without merging.
func Load(cfgFile string) (*Config, error) {
	v := viper.New()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.AddConfigPath("etc")
		v.SetConfigName("config")
		v.SetConfigType("yaml")
	}

	v.AutomaticEnv()
	// Environment variable mapping: DATABASE__DSN -> database.dsn, REDIS__ADDR -> redis.addr.
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "__"))

	// Defaults (must be set before ReadInConfig).
	v.SetDefault("server.env", "production")
	v.SetDefault("auth.access_token_ttl_seconds", 3600)
	v.SetDefault("auth.refresh_token_ttl", 30*24*time.Hour)
	v.SetDefault("auth.oauth2_state_ttl", 10*time.Minute)
	v.SetDefault("auth.allowed_redirect_uris", []string{})
	v.SetDefault("providers.mock.enabled", true)
	v.SetDefault("providers.mock.url", "http://localhost:10060")
	v.SetDefault("providers.mock.client_id", "test-client")
	v.SetDefault("providers.mock.client_secret", "test-client-secret")
	v.SetDefault("providers.github.enabled", false)
	v.SetDefault("providers.github.client_id", "")
	v.SetDefault("providers.github.client_secret", "")
	v.SetDefault("providers.github.scope", "read:user user:email")
	v.SetDefault("providers.google.enabled", false)
	v.SetDefault("providers.google.client_id", "")
	v.SetDefault("providers.google.client_secret", "")
	v.SetDefault("providers.google.scope", "openid email profile")
	v.SetDefault("providers.http_timeout", 10*time.Second)
	v.SetDefault("cors.allow_origins", []string{})
	v.SetDefault("server.shutdown_timeout", 10*time.Second)
	v.SetDefault("debug.enabled", true)
	v.SetDefault("debug.goroutine_leak_threshold", 1000)
	v.SetDefault("server.rpc_timeout", 10*time.Second)
	v.SetDefault("worker.heartbeat_sec", 10)
	v.SetDefault("worker.ttl_sec", 60)
	v.SetDefault("worker.idle_timeout", 5*time.Minute)
	v.SetDefault("worker.stream_block", 500*time.Millisecond)
	v.SetDefault("worker.max_len_approx", 10000)
	v.SetDefault("worker.background_concurrency", 5)
	v.SetDefault("worker.context_tokens_limit", 25000)
	v.SetDefault("worker.auto_compact_buffer_tokens", 13000)
	v.SetDefault("worker.max_output_tokens_for_summary", 20000)
	v.SetDefault("worker.checkpoint_ttl", 24*time.Hour)
	v.SetDefault("worker.stream_chunk_ttl", 5*time.Minute)
	v.SetDefault("worker.interrupt_answer_ttl", 10*time.Minute)
	v.SetDefault("worker.orphan_trigger_ttl", 24*time.Hour)
	v.SetDefault("worker.lock_ttl_sec", 120)
	v.SetDefault("worker.token_counter_mode", "heuristic")
	v.SetDefault("worker.enable_strategic_cache_breakpoints", true)
	v.SetDefault("llm.api_key", "")
	v.SetDefault("llm.thinking_budget_tokens", 50000)
	v.SetDefault("llm.reasoning_effort", "medium")
	v.SetDefault("llm.retry_max_attempts", 0)
	v.SetDefault("llm.retry_base_delay", 1*time.Second)
	// Default pricing: Claude 3.5 Sonnet (USD per million tokens).
	v.SetDefault("llm.pricing.input_per_million", 3.0)
	v.SetDefault("llm.pricing.output_per_million", 15.0)
	v.SetDefault("llm.pricing.cached_read_per_million", 0.3)
	v.SetDefault("llm.pricing.cached_write_per_million", 3.75)
	v.SetDefault("llm.pricing.reasoning_per_million", 0.0)
	v.SetDefault("api.query_default_limit", 50)
	v.SetDefault("api.query_max_limit", 100)
	v.SetDefault("tracing.enabled", false)
	v.SetDefault("tracing.endpoint", "localhost:4317")
	v.SetDefault("tracing.sample_rate", 1.0)
	v.SetDefault("log.llm_payload", false)
	// asynq defaults
	v.SetDefault("asynq.concurrency", 10)
	v.SetDefault("asynq.queue", "loop")
	v.SetDefault("asynq.recovery_interval", 1*time.Minute)
	v.SetDefault("asynq.stale_threshold", 5*time.Minute)
	v.SetDefault("asynq.retry_max", 3)
	v.SetDefault("asynq.retry_timeout", 30*time.Second)
	v.SetDefault("asynq.health_check_interval", 30*time.Second)

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	// When --config is not specified, auto-merge etc/config.local.yaml (if it exists).
	if cfgFile == "" {
		localV := viper.New()
		localV.AddConfigPath("etc")
		localV.SetConfigName("config.local")
		localV.SetConfigType("yaml")
		if err := localV.ReadInConfig(); err == nil {
			for _, key := range localV.AllKeys() {
				v.Set(key, localV.Get(key))
			}
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Expand environment variable references in sensitive configuration fields (${VAR_NAME} form).
	// Only applies to sensitive fields to prevent other config items from inadvertently using environment variables.
	expandEnvVars(&cfg)

	return &cfg, nil
}
