package config

import (
	"time"
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
	WebSearch WebSearchConfig `mapstructure:"web_search"`
	WebFetch  WebFetchConfig  `mapstructure:"web_fetch"`
	Storage   StorageConfig   `mapstructure:"storage"`
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

// WebSearchConfig holds the web search subsystem configuration.
// Controls search providers, proxies, circuit breaking, rate limiting, and retry.
type WebSearchConfig struct {
	// Enabled controls whether web search is active.
	Enabled bool `mapstructure:"enabled"`

	// BalancerType selects the load balancing strategy: "round_robin" or "weighted".
	BalancerType string `mapstructure:"balancer_type"`

	// Providers is the list of search provider configurations.
	Providers []WebSearchProviderConfig `mapstructure:"providers"`

	// Proxies is the list of proxy configurations for outbound search requests.
	Proxies []WebSearchProxyConfig `mapstructure:"proxies"`

	// CircuitBreaker holds circuit breaker parameters.
	CircuitBreaker WebSearchCircuitBreakerConfig `mapstructure:"circuit_breaker"`

	// RateLimiter holds rate limiter parameters.
	RateLimiter WebSearchRateLimiterConfig `mapstructure:"rate_limiter"`

	// Retry holds retry and backoff parameters.
	Retry WebSearchRetryConfig `mapstructure:"retry"`

	// GlobalTimeout is the per-search request timeout.
	GlobalTimeout time.Duration `mapstructure:"global_timeout"`

	// ProxyHealthURL is the URL used for proxy health checks.
	ProxyHealthURL string `mapstructure:"proxy_health_url"`

	// ProxyCheckInterval is the proxy health check interval.
	ProxyCheckInterval time.Duration `mapstructure:"proxy_check_interval"`

	// Redis holds the dedicated Redis configuration for distributed circuit breakers
	// and rate limiters. When Addr is empty, distributed features are disabled.
	Redis WebSearchRedisConfig `mapstructure:"redis"`
}

// WebSearchProviderConfig holds a single search provider configuration.
type WebSearchProviderConfig struct {
	// Name is the provider identifier (e.g., "tavily", "bing", "searxng").
	Name string `mapstructure:"name"`

	// Type is the provider type: "tavily", "bing", "searxng", "duckduckgo".
	Type string `mapstructure:"type"`

	// Weight is the load balancer weight (0-100).
	Weight int `mapstructure:"weight"`

	// Enabled controls whether this provider is active.
	Enabled bool `mapstructure:"enabled"`

	// Region is the search region (e.g., "us-en", "cn-zh"). Used by DuckDuckGo.
	Region string `mapstructure:"region"`

	// APIKey is the provider API key. Used by Bing.
	APIKey string `mapstructure:"api_key"`

	// BaseURL is the provider base URL. Used by SearXNG.
	BaseURL string `mapstructure:"base_url"`

	// MaxResults is the maximum number of results per query.
	MaxResults int `mapstructure:"max_results"`

	// Timeout is the per-provider request timeout.
	Timeout time.Duration `mapstructure:"timeout"`

	// Language is the preferred language for results. Used by SearXNG.
	Language string `mapstructure:"language"`

	// Market is the market code. Used by Bing (e.g., "en-US").
	Market string `mapstructure:"market"`

	// Endpoint is the API endpoint URL. Used by Bing.
	Endpoint string `mapstructure:"endpoint"`

	// SearchDepth controls the depth of search: "basic" or "advanced". Used by Tavily.
	SearchDepth string `mapstructure:"search_depth"`

	// IncludeAnswer whether to include a generated answer summary. Used by Tavily.
	IncludeAnswer bool `mapstructure:"include_answer"`
}

// WebSearchProxyConfig holds a single proxy configuration.
type WebSearchProxyConfig struct {
	// URL is the proxy URL (e.g., "socks5://user:pass@host:port").
	URL string `mapstructure:"url"`

	// Type is the proxy type: "socks5", "http", "https", "direct".
	Type string `mapstructure:"type"`

	// Region is the proxy region (e.g., "us", "cn", "global").
	Region string `mapstructure:"region"`

	// Priority is the proxy priority (1-10, higher is better).
	Priority int `mapstructure:"priority"`
}

// WebSearchCircuitBreakerConfig holds circuit breaker parameters.
type WebSearchCircuitBreakerConfig struct {
	// FailureThreshold is the failure percentage (0-100) to trigger circuit open.
	FailureThreshold int `mapstructure:"failure_threshold"`

	// OpenTimeout is the wait time before transitioning from open to half-open.
	OpenTimeout time.Duration `mapstructure:"open_timeout"`

	// HalfOpenMaxRequests is the max probe requests allowed in half-open state.
	HalfOpenMaxRequests int `mapstructure:"half_open_max_requests"`

	// WindowSize is the sliding window size (number of requests).
	WindowSize int `mapstructure:"window_size"`

	// WindowDuration is the sliding window time dimension.
	WindowDuration time.Duration `mapstructure:"window_duration"`
}

// WebSearchRateLimiterConfig holds rate limiter parameters.
type WebSearchRateLimiterConfig struct {
	// Rate is the allowed requests per second.
	Rate float64 `mapstructure:"rate"`

	// Burst is the maximum burst capacity.
	Burst int `mapstructure:"burst"`
}

// WebSearchRetryConfig holds retry and backoff parameters.
type WebSearchRetryConfig struct {
	// MaxRetries is the maximum retry attempts on failure.
	MaxRetries int `mapstructure:"max_retries"`

	// RetryDelay is the initial retry delay.
	RetryDelay time.Duration `mapstructure:"retry_delay"`

	// BackoffFactor is the exponential backoff multiplier.
	BackoffFactor float64 `mapstructure:"backoff_factor"`
}

// WebSearchRedisConfig holds the dedicated Redis configuration for distributed
// circuit breakers and rate limiters.
type WebSearchRedisConfig struct {
	// Addr is the Redis address. Empty disables distributed features.
	Addr string `mapstructure:"addr"`

	// Password is the Redis password.
	Password string `mapstructure:"password"`

	// DB is the Redis database number.
	DB int `mapstructure:"db"`
}

// WebFetchConfig holds the web fetch subsystem configuration.
// Controls fetching, caching, security, and concurrency limits.
type WebFetchConfig struct {
	// Enabled controls whether web fetch is active.
	Enabled bool `mapstructure:"enabled"`

	// MaxConcurrency is the global max concurrent fetches.
	MaxConcurrency int `mapstructure:"max_concurrency"`

	// MaxDomainConcurrency is the per-domain max concurrent fetches.
	MaxDomainConcurrency int `mapstructure:"max_domain_concurrency"`

	// CacheTTL is the cache time-to-live.
	CacheTTL time.Duration `mapstructure:"cache_ttl"`

	// MaxURLLength is the max URL length.
	MaxURLLength int `mapstructure:"max_url_length"`

	// MaxContentSize is the max content bytes.
	MaxContentSize int64 `mapstructure:"max_content_size"`

	// FetchTimeout is the request timeout.
	FetchTimeout time.Duration `mapstructure:"fetch_timeout"`

	// MaxRedirects is the max redirects to follow.
	MaxRedirects int `mapstructure:"max_redirects"`

	// LLMExtractThresholdBytes is the content size threshold to trigger LLM extraction.
	LLMExtractThresholdBytes int `mapstructure:"llm_extract_threshold"`

	// LLMMaxTokens is the max output tokens for LLM extraction.
	LLMMaxTokens int `mapstructure:"llm_max_tokens"`

	// MaxLLMExtractPerSession is the daily LLM extraction limit per session.
	MaxLLMExtractPerSession int `mapstructure:"max_llm_per_session"`

	// RateLimit holds dual-layer rate limiting configuration.
	RateLimit WebFetchRateLimitConfig `mapstructure:"rate_limit"`

	// RobotsCacheTTL is the robots.txt cache TTL.
	RobotsCacheTTL time.Duration `mapstructure:"robots_cache_ttl"`

	// UserAgent is the User-Agent header.
	UserAgent string `mapstructure:"user_agent"`

	// RespectRobotsTxt controls whether to respect robots.txt.
	RespectRobotsTxt bool `mapstructure:"respect_robots_txt"`

	// PreApprovedDomains is the list of pre-approved domains.
	PreApprovedDomains []string `mapstructure:"pre_approved_domains"`

	// BlockedDomains is the list of blocked domains.
	BlockedDomains []string `mapstructure:"blocked_domains"`

	// Redis holds the dedicated Redis configuration for caching and rate limiting.
	// When Addr is empty, the shared redis client is used.
	Redis WebFetchRedisConfig `mapstructure:"redis"`
}

// WebFetchRateLimitConfig holds dual-layer rate limiting configuration for web fetch.
type WebFetchRateLimitConfig struct {
	// GlobalRPS is the global requests per second.
	GlobalRPS float64 `mapstructure:"global_rps"`
	// GlobalBurst is the global burst size.
	GlobalBurst int `mapstructure:"global_burst"`
	// DomainRPS is the per-domain requests per second.
	DomainRPS float64 `mapstructure:"domain_rps"`
	// DomainBurst is the per-domain burst size.
	DomainBurst int `mapstructure:"domain_burst"`
}

// WebFetchRedisConfig holds the dedicated Redis configuration for web fetch
// caching and rate limiting.
type WebFetchRedisConfig struct {
	// Addr is the Redis address. Empty falls back to the shared redis client.
	Addr string `mapstructure:"addr"`

	// Password is the Redis password.
	Password string `mapstructure:"password"`

	// DB is the Redis database number.
	DB int `mapstructure:"db"`
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

// StorageConfig holds object storage (rtc-oss3) configuration.
// When Backend is empty, OSS3 is completely disabled (S3 endpoint not started, STS API returns 501).
type StorageConfig struct {
	// Backend is the storage backend type: "minio" (reserved: "oss", "cos", "s3").
	// Empty disables OSS3 functionality.
	Backend string `mapstructure:"backend"`

	// MinIO holds MinIO backend configuration.
	MinIO MinIOConfig `mapstructure:"minio"`

	// S3Endpoint holds S3-compatible endpoint configuration.
	S3Endpoint S3EndpointConfig `mapstructure:"s3_endpoint"`

	// Quota holds storage quota configuration.
	Quota QuotaConfig `mapstructure:"quota"`

	// RateLimit holds S3 API rate limiting configuration.
	RateLimit RateLimitConfig `mapstructure:"rate_limit"`

	// Credential holds temporary credential configuration.
	Credential CredentialConfig `mapstructure:"credential"`

	// Cleanup holds periodic cleanup task configuration.
	Cleanup CleanupConfig `mapstructure:"cleanup"`

	// Encryption holds SessionToken encryption configuration.
	Encryption EncryptionConfig `mapstructure:"encryption"`
}

// IsEnabled returns true if a storage backend is configured.
// When backend is empty, OSS3 is completely disabled:
//   - S3 HTTP server is not started (no :9000 port)
//   - STS API endpoints return 501 Not Implemented
//   - Readyz "storage" field returns "not configured" (non-blocking)
//   - Cleanup goroutine is not started
func (c *StorageConfig) IsEnabled() bool {
	return c.Backend != ""
}

// MinIOConfig holds MinIO backend connection configuration.
type MinIOConfig struct {
	// Endpoint is the MinIO server endpoint (e.g., "http://localhost:9000").
	// Used for server-to-MinIO communication (internal network).
	Endpoint string `mapstructure:"endpoint"`

	// PublicURL is the public-facing S3 endpoint URL (e.g., "http://localhost:29000").
	// Used for generating presigned URLs that clients will access (public network).
	// When empty, falls back to Endpoint.
	PublicURL string `mapstructure:"public_url"`

	// AccessKey is the MinIO access key (aligned with docker-compose MINIO_ROOT_USER).
	AccessKey string `mapstructure:"access_key"`

	// SecretKey is the MinIO secret key (aligned with docker-compose MINIO_ROOT_PASSWORD).
	SecretKey string `mapstructure:"secret_key"`

	// Bucket is the bucket name (default "rtc-agent").
	Bucket string `mapstructure:"bucket"`

	// Region is the S3 region for presigned URL generation (e.g., "us-east-1").
	// Used by publicClient to avoid GetBucketLocation calls. Default "us-east-1".
	Region string `mapstructure:"region"`

	// UseSSL enables HTTPS for MinIO connection.
	UseSSL bool `mapstructure:"use_ssl"`

	// HTTP Transport tuning (R7 Review H1).
	// MaxIdleConns is the total idle connections across all hosts. Default 1024.
	MaxIdleConns int `mapstructure:"max_idle_conns"`

	// MaxIdleConnsPerHost is the idle connections per MinIO endpoint. Default 100.
	MaxIdleConnsPerHost int `mapstructure:"max_idle_conns_per_host"`

	// IdleConnTimeout is the timeout for idle connections. Default 90s.
	IdleConnTimeout time.Duration `mapstructure:"idle_conn_timeout"`

	// Retry tuning (R7 Review H5).
	// minio-go defaults: 10 retries with exponential backoff.
	// Override to 3 retries to fail faster and avoid long tail latency.

	// MaxRetries is the max retry attempts per request. Default 3.
	MaxRetries int `mapstructure:"max_retries"`

	// RetryTimeout is the total retry time budget. Default 30s.
	RetryTimeout time.Duration `mapstructure:"retry_timeout"`
}

// S3EndpointConfig holds S3-compatible endpoint configuration.
type S3EndpointConfig struct {
	// Host is the listen address (e.g., "0.0.0.0").
	Host string `mapstructure:"host"`

	// Port is the S3 endpoint port (default 9000).
	Port int `mapstructure:"port"`

	// TLSCert is the TLS certificate path (PEM format). Optional.
	// When both TLSCert and TLSKey are set, S3 endpoint uses HTTPS.
	TLSCert string `mapstructure:"tls_cert"`

	// TLSKey is the TLS private key path (PEM format). Optional.
	TLSKey string `mapstructure:"tls_key"`

	// AllowedOrigins is the CORS whitelist (default ["*"]).
	AllowedOrigins []string `mapstructure:"allowed_origins"`

	// Region is the S3 region for SigV4 signing (default "us-east-1").
	Region string `mapstructure:"region"`
}

// QuotaConfig holds storage quota configuration.
type QuotaConfig struct {
	// MaxFileSizeBytes is the maximum single file size. Default 100MB.
	MaxFileSizeBytes int64 `mapstructure:"max_file_size_bytes"`

	// MaxUserQuotaBytes is the maximum total storage per user. Default 1GB.
	MaxUserQuotaBytes int64 `mapstructure:"max_user_quota_bytes"`

	// MaxConcurrentUploads is the maximum concurrent multipart uploads per user. Default 10.
	MaxConcurrentUploads int `mapstructure:"max_concurrent_uploads"`

	// PendingTTL is the two-phase reserve expiry duration. Default 5m.
	PendingTTL time.Duration `mapstructure:"pending_ttl"`
}

// RateLimitConfig holds S3 API rate limiting configuration.
type RateLimitConfig struct {
	// RequestsPerMinute is the per-user S3 API rate limit. Default 60.
	RequestsPerMinute int `mapstructure:"requests_per_minute"`
}

// CredentialConfig holds temporary credential configuration.
type CredentialConfig struct {
	// AccessTokenTTL is the access token validity duration. Default 1h.
	AccessTokenTTL time.Duration `mapstructure:"access_token_ttl"`

	// SessionTokenTTL is the session token validity duration. Default 1h.
	SessionTokenTTL time.Duration `mapstructure:"session_token_ttl"`

	// PresignedURLTTL is the presigned URL validity duration. Default 1h.
	PresignedURLTTL time.Duration `mapstructure:"presigned_url_ttl"`
}

// CleanupConfig holds periodic cleanup task configuration.
type CleanupConfig struct {
	// Interval is the cleanup task execution interval. Default 1h.
	Interval time.Duration `mapstructure:"interval"`

	// MultipartExpiry is the incomplete multipart upload expiry duration. Default 24h.
	MultipartExpiry time.Duration `mapstructure:"multipart_expiry"`

	// CredentialExpiry is the expired credential cleanup threshold. Default 24h.
	CredentialExpiry time.Duration `mapstructure:"credential_expiry"`

	// Orphan holds orphan object cleanup configuration.
	Orphan OrphanCleanupConfig `mapstructure:"orphan"`
}

// OrphanCleanupConfig holds MinIO orphan object cleanup configuration.
// Orphan objects are MinIO objects with no corresponding DB record, typically
// caused by DB failures after successful MinIO upload.
type OrphanCleanupConfig struct {
	// Enabled controls whether orphan cleanup is active. Default false.
	// Orphan cleanup performs full-bucket scans and should be enabled only
	// after verifying the reconciler does not interfere with normal operations.
	Enabled bool `mapstructure:"enabled"`

	// BatchSize is the number of MinIO objects scanned per batch.
	// Larger batches reduce round-trips but increase memory usage. Default 500.
	BatchSize int `mapstructure:"batch_size"`

	// CooldownPeriod is the minimum age an object must have before it can be
	// deleted. Objects newer than this are skipped to avoid deleting objects
	// that are still being uploaded or committed. Default 1h.
	CooldownPeriod time.Duration `mapstructure:"cooldown_period"`

	// DryRun when true logs orphans without deleting them. Default true.
	// Set to false only after verifying the reconciler identifies correct orphans.
	DryRun bool `mapstructure:"dry_run"`
}

// EncryptionConfig holds SessionToken encryption configuration.
type EncryptionConfig struct {
	// SessionTokenKey is the AES-256 key (32 bytes) for SessionToken encryption.
	// Injected via environment variable OSS3_SESSION_TOKEN_KEY.
	SessionTokenKey string `mapstructure:"session_token_key"`
}
