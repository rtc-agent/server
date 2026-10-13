package config

import (
	"time"
)

// Config is the top-level application configuration, aggregating all sub-module configs.
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Redis         RedisConfig         `mapstructure:"redis"`
	Log           LogConfig           `mapstructure:"log"`
	Auth          AuthConfig          `mapstructure:"auth"`
	Providers     ProvidersConfig     `mapstructure:"providers"`
	CORS          CORSConfig          `mapstructure:"cors"`
	Worker        WorkerConfig        `mapstructure:"worker"`
	LLM           LLMConfig           `mapstructure:"llm"`
	API           APIConfig           `mapstructure:"api"`
	Tracing       TracingConfig       `mapstructure:"tracing"`
	Asynq         AsynqConfig         `mapstructure:"asynq"`
	Metrics       MetricsConfig       `mapstructure:"metrics"`
	Debug         DebugConfig         `mapstructure:"debug"`
	WebSearch     WebSearchConfig     `mapstructure:"web_search"`
	WebFetch      WebFetchConfig      `mapstructure:"web_fetch"`
	Storage       StorageConfig       `mapstructure:"storage"`
	TokenExchange TokenExchangeConfig `mapstructure:"token_exchange"`
	Security      SecurityConfig      `mapstructure:"security"`
}

// SecurityConfig holds security-related configuration for the main server.
type SecurityConfig struct {
	// CSP holds Content Security Policy configuration.
	CSP CSPConfig `mapstructure:"csp"`
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
	// Enabled controls whether debug endpoints are active. Defaults to false.
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

	// ClientQueueMaxSize is the maximum size of client's message queue in bytes.
	// When exceeded, Centrifuge closes the client connection with DisconnectSlow.
	// This limits RPC response size since replies go through the message queue.
	// Default 52428800 (50MB).
	ClientQueueMaxSize int `mapstructure:"client_queue_max_size"`
}

// DatabaseConfig holds database connection configuration.
type DatabaseConfig struct {
	DSN string `mapstructure:"dsn"`
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
	OAuth2StateTTL        time.Duration `mapstructure:"oauth2_state_ttl"`  // Default 5 minutes.
	// AllowedRedirectURIs is the OAuth2 redirect_uri whitelist.
	// Empty means no restriction (development only); production must configure explicitly.
	AllowedRedirectURIs []string `mapstructure:"allowed_redirect_uris"`

	// RefreshTokenReuseGracePeriod is the time window after token revocation during which
	// a reused token is considered a concurrent refresh attempt rather than token theft.
	// This prevents aggressive revocation of all user tokens when multiple concurrent
	// refresh requests occur (e.g., multiple browser tabs, parallel API calls).
	// Default 30s. Shorter values (10-30s) increase security; longer values reduce false positives.
	RefreshTokenReuseGracePeriod time.Duration `mapstructure:"refresh_token_reuse_grace_period"`
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

	// StreamTTLCheckInterval is the number of AppendChunk calls between TTL checks.
	// Default 100. Checking every 100 calls provides a good balance between visibility
	// into TTL drift and Redis load reduction. For a typical stream of 500 chunks,
	// this reduces TTL checks from 500 to 5. Lower values increase Redis load but
	// provide faster detection of TTL issues; higher values reduce load but delay
	// detection of stuck streams or TTL consistency problems.
	StreamTTLCheckInterval int `mapstructure:"stream_ttl_check_interval"`

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

// LLMConfig holds LLM model configuration.
//
// NOTE: Currently only Claude protocol is fully supported and tested.
// TODO: OpenAI protocol support is incomplete and not recommended for production use.
// The OpenAI adapter has not been thoroughly tested with our message format,
// especially for multimodal content (images, files). Only use "claude" provider
// in production until OpenAI support is fully validated.
type LLMConfig struct {
	// Provider is the model provider. Currently only "claude" is fully supported.
	// "openai" is available but not recommended for production use.
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

	// RateLimit holds per-user HTTP API rate limiting configuration.
	RateLimit APIRateLimitConfig `mapstructure:"rate_limit"`

	// IPRateLimit holds per-IP HTTP API rate limiting configuration.
	IPRateLimit IPRateLimitConfig `mapstructure:"ip_rate_limit"`
}

// APIRateLimitConfig holds per-user rate limiting configuration.
type APIRateLimitConfig struct {
	// RequestsPerSecond is the allowed requests per second per user. Default 50.
	RequestsPerSecond float64 `mapstructure:"requests_per_second"`

	// BurstSize is the maximum burst size. Default 100.
	BurstSize int `mapstructure:"burst_size"`
}

// IPRateLimitConfig holds per-IP rate limiting configuration.
type IPRateLimitConfig struct {
	// RequestsPerSecond is the allowed requests per second per IP. Default 5.
	RequestsPerSecond float64 `mapstructure:"requests_per_second"`

	// BurstSize is the maximum burst size. Default 10.
	BurstSize int `mapstructure:"burst_size"`

	// LimiterTTLSec is the TTL for limiter entries in seconds. Default 3600 (1 hour).
	LimiterTTLSec int `mapstructure:"limiter_ttl_sec"`

	// TokenExchange holds stricter rate limiting for the Token Exchange endpoint.
	// Token Exchange involves external JWT verification and is a high-value target
	// for brute-force attacks, so it uses tighter limits than the general OAuth2 endpoints.
	TokenExchange TokenExchangeIPRateLimitConfig `mapstructure:"token_exchange"`
}

// TokenExchangeIPRateLimitConfig holds rate limiting configuration specifically
// for the RFC 8693 Token Exchange endpoint (POST /oauth2/token with grant_type=token-exchange).
//
// These limits are intentionally stricter than the general IPRateLimit because:
//   - Each request triggers external JWT signature verification (CPU-intensive)
//   - The endpoint is a high-value target for brute-force attacks against trusted issuers
//   - Legitimate clients rarely exceed 1-2 requests per second
type TokenExchangeIPRateLimitConfig struct {
	// RequestsPerSecond is the allowed requests per second per IP. Default 3.
	RequestsPerSecond float64 `mapstructure:"requests_per_second"`

	// BurstSize is the maximum burst size. Default 5.
	BurstSize int `mapstructure:"burst_size"`
}

// TracingConfig holds OpenTelemetry distributed tracing configuration.
type TracingConfig struct {
	// Enabled controls whether tracing is active.
	Enabled bool `mapstructure:"enabled"`

	// Endpoint is the OTLP endpoint (e.g., "localhost:4317").
	Endpoint string `mapstructure:"endpoint"`

	// SampleRate is the sampling rate (0.0 - 1.0). Default 1.0 (full sampling).
	SampleRate float64 `mapstructure:"sample_rate"`

	// Insecure controls whether to use an unencrypted connection to the OTLP endpoint.
	// Default false (use TLS). Set to true only for development environments.
	Insecure bool `mapstructure:"insecure"`

	// ServiceVersion is the version of the service (e.g., "1.0.0").
	// Optional but recommended for multi-version deployments and canary releases.
	// Defaults to "unknown" if not specified.
	ServiceVersion string `mapstructure:"service_version"`

	// Environment is the deployment environment (e.g., "production", "staging", "development").
	// Optional but helps distinguish traces from different environments.
	// Defaults to cfg.Server.Env or "development" if not specified.
	Environment string `mapstructure:"environment"`
}
