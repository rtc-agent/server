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

// WorkerConfig Worker 生命周期与 TurnLoop 配置
type WorkerConfig struct {
	WorkerID              string        `mapstructure:"worker_id"`
	Host                  string        `mapstructure:"host"`
	Version               string        `mapstructure:"version"`
	HeartbeatSec          int           `mapstructure:"heartbeat_sec"`          // 默认 10
	TTLSec                int           `mapstructure:"ttl_sec"`                // 默认 60
	IdleTimeout           time.Duration `mapstructure:"idle_timeout"`           // 默认 5m
	StreamBlock           time.Duration `mapstructure:"stream_block"`           // 默认 500ms
	MaxLenApprox          int64         `mapstructure:"max_len_approx"`         // 默认 10000
	BackgroundConcurrency int           `mapstructure:"background_concurrency"` // 默认 5
	SystemPrompt          string        `mapstructure:"system_prompt"`          // agent 系统提示词

	// ContextTokensLimit 触发上下文压缩的 token 阈值（可选）
	// 默认 25000（约为模型上下文窗口的 20%）
	ContextTokensLimit int `mapstructure:"context_tokens_limit"`

	// AutoCompactBufferTokens 自动压缩缓冲 token 数（可选）
	// 用于计算实际触发阈值：ContextTokensLimit - AutoCompactBufferTokens
	// 默认 13000
	AutoCompactBufferTokens int `mapstructure:"auto_compact_buffer_tokens"`

	// MaxOutputTokensForSummary 压缩时最大输出 token 数（可选）
	// 默认 20000
	MaxOutputTokensForSummary int `mapstructure:"max_output_tokens_for_summary"`

	// CheckpointTTL eino checkpoint 在 Redis 中的存活时间
	// 默认 24h。较长的 TTL 提高崩溃恢复窗口，但增加 Redis 内存压力
	CheckpointTTL time.Duration `mapstructure:"checkpoint_ttl"`

	// StreamChunkTTL 流式消息 chunk 在 Redis 中的存活时间
	// 默认 15m。chunks 是短生命周期数据，生成完成后即删除
	// 注意：较长的 thinking/reasoning 流可能需要更长的 TTL，避免 chunk 丢失
	StreamChunkTTL time.Duration `mapstructure:"stream_chunk_ttl"`

	// InterruptAnswerTTL interrupt 答案在 Redis 中的存活时间
	// 默认 10m
	InterruptAnswerTTL time.Duration `mapstructure:"interrupt_answer_ttl"`

	// OrphanTriggerTTL RTC orphan recovery 去重标记的存活时间
	// 默认 24h
	OrphanTriggerTTL time.Duration `mapstructure:"orphan_trigger_ttl"`

	// LockTTLSeconds rtc-queue session 锁的 TTL（秒）
	// 默认 120
	LockTTLSeconds int `mapstructure:"lock_ttl_sec"`

	// CacheHitRateWarnThreshold 缓存命中率告警阈值（可选）
	// Session 累计缓存命中率 = TotalCachedReadTokens / (TotalCachedReadTokens + TotalInputTokens)
	// 低于此阈值时输出 warn 日志。负数表示禁用告警。
	// 默认 0.88（88%）
	CacheHitRateWarnThreshold float64 `mapstructure:"cache_hit_rate_warn_threshold"`

	// TokenCounterMode 控制 token 计数策略（可选）
	// 选项：
	//   - "heuristic"（默认）：快速，约 4 字符/token，适合英文文本
	//   - "tokenizer"：精确，使用 cl100k_base 编码（tiktoken-go），对中文精度提升 3-4 倍
	// 注意：tokenizer 模式首次加载约 50-200ms，内存占用约 5MB
	TokenCounterMode string `mapstructure:"token_counter_mode"`

	// EnableStrategicCacheBreakpoints 启用战略性缓存断点（可选）
	// 通过在关键位置设置缓存断点，保护稳定内容免受 microcompact 或工具结果预算修改导致的缓存失效
	// 启用后，microcompact 后的缓存命中率从 0% 提升至 75%，输入成本降低约 69%
	// 默认 true
	EnableStrategicCacheBreakpoints bool `mapstructure:"enable_strategic_cache_breakpoints"`
}

// LLMConfig LLM 模型配置（支持 Claude 和 OpenAI 协议）
type LLMConfig struct {
	// Provider 模型提供商: "claude" 或 "openai"
	Provider string `mapstructure:"provider"`

	// APIKey API 密钥
	APIKey string `mapstructure:"api_key"`

	// BaseURL 自定义 API 端点（可选，用于代理或企业部署）
	BaseURL string `mapstructure:"base_url"`

	// Model 模型名称（如 "claude-3-5-sonnet-20241022", "gpt-4o"）
	Model string `mapstructure:"model"`

	// MaxTokens 最大输出 token 数（Claude 必填，OpenAI 可选）
	MaxTokens int `mapstructure:"max_tokens"`

	// Temperature 采样温度（OpenAI 可选，0.0-2.0）
	Temperature *float32 `mapstructure:"temperature"`

	// Timeout API 请求超时时间（可选）
	Timeout time.Duration `mapstructure:"timeout"`

	// ThinkingBudgetTokens Claude thinking 模式的 token 预算（可选）
	// 仅对支持 thinking 的 Claude 模型生效，默认 50000
	ThinkingBudgetTokens int64 `mapstructure:"thinking_budget_tokens"`

	// ReasoningEffort OpenAI reasoning 模型的推理力度（可选）
	// 可选值: "low", "medium", "high"，默认 "medium"
	ReasoningEffort string `mapstructure:"reasoning_effort"`

	// RetryMaxAttempts 模型调用失败时的最大重试次数（可选）
	// 默认 0（不重试）。建议生产环境设置为 3
	RetryMaxAttempts int `mapstructure:"retry_max_attempts"`

	// RetryBaseDelay 重试的基础退避时间（可选）
	// 默认 1s。实际退避时间 = RetryBaseDelay * 2^(attempt-1)，即指数退避
	RetryBaseDelay time.Duration `mapstructure:"retry_base_delay"`

	// Pricing 模型定价配置（可选，用于成本计算）
	// 未配置时使用默认价格（Claude 3.5 Sonnet）
	Pricing *ModelPricingConfig `mapstructure:"pricing"`
}

// ModelPricingConfig 模型价格配置（USD per million tokens）
type ModelPricingConfig struct {
	// InputPerMillion 正常 input token 价格（USD）
	InputPerMillion float64 `mapstructure:"input_per_million"`

	// OutputPerMillion output token 价格（USD）
	OutputPerMillion float64 `mapstructure:"output_per_million"`

	// CachedReadPerMillion cache read（cache hit）价格（USD）
	// 通常为 input 的 10%
	CachedReadPerMillion float64 `mapstructure:"cached_read_per_million"`

	// CachedWritePerMillion cache write（cache creation）价格（USD）
	// 通常为 input 的 125%
	CachedWritePerMillion float64 `mapstructure:"cached_write_per_million"`

	// ReasoningPerMillion reasoning（thinking）token 价格（USD）
	ReasoningPerMillion float64 `mapstructure:"reasoning_per_million"`
}

// APIConfig API 层配置（分页、限流等）
type APIConfig struct {
	// QueryDefaultLimit 分页查询默认每页条数，默认 50
	QueryDefaultLimit int `mapstructure:"query_default_limit"`

	// QueryMaxLimit 分页查询最大每页条数，默认 100
	QueryMaxLimit int `mapstructure:"query_max_limit"`
}

// TracingConfig OpenTelemetry 分布式追踪配置
type TracingConfig struct {
	// Enabled 是否启用 tracing
	Enabled bool `mapstructure:"enabled"`

	// Endpoint OTLP endpoint (例如: "localhost:4317")
	Endpoint string `mapstructure:"endpoint"`

	// SampleRate 采样率 (0.0 - 1.0)，默认 1.0（全量采样）
	SampleRate float64 `mapstructure:"sample_rate"`
}

// Load 加载配置。使用局部 viper 实例，不污染全局状态，可安全并行测试。
//
// 配置合并策略（无 --config 时）：
//  1. 加载 etc/config.yaml 作为基线
//  2. 若 etc/config.local.yaml 存在，合并覆盖基线（仅写差异项）
//  3. config.local.yaml 应加入 .gitignore，用于本地个人配置
//
// 使用 --config 时，仅加载指定文件，不做合并。
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
	// 环境变量映射：DATABASE__DSN → database.dsn, REDIS__ADDR → redis.addr
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "__"))

	// 默认值（必须在 ReadInConfig 之前设置）
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
	v.SetDefault("llm.thinking_budget_tokens", 50000)
	v.SetDefault("llm.reasoning_effort", "medium")
	v.SetDefault("llm.retry_max_attempts", 0)
	v.SetDefault("llm.retry_base_delay", 1*time.Second)
	// 默认定价：Claude 3.5 Sonnet（USD per million tokens）
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

	// 无 --config 时，自动合并 etc/config.local.yaml（若存在）
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

	// 展开敏感配置项中的环境变量引用（${VAR_NAME} 形式）。
	// 仅对敏感字段生效，避免其他配置项误用环境变量引入安全隐患。
	expandEnvVars(&cfg)

	return &cfg, nil
}
