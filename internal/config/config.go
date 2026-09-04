package config

import (
	"time"

	"github.com/spf13/viper"
)

// Config 应用顶层配置，聚合所有子模块配置。
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
}

// ServerConfig HTTP/WebSocket 服务器监听地址配置。
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`

	// ShutdownTimeout 优雅停机超时时间（等待 in-flight 请求完成）
	// 默认 10s
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`

	// RPCTimeout RPC 处理器上下文超时时间
	// 默认 10s
	RPCTimeout time.Duration `mapstructure:"rpc_timeout"`
}

// DatabaseConfig 数据库连接与迁移配置。
type DatabaseConfig struct {
	DSN         string `mapstructure:"dsn"`
	AutoMigrate bool   `mapstructure:"auto_migrate"`
}

// LogConfig 日志级别配置。
type LogConfig struct {
	Level string `mapstructure:"level"`
}

// RedisConfig Redis 连接配置
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// AuthConfig JWT / 令牌相关配置
type AuthConfig struct {
	JWTSecret             string        `mapstructure:"jwt_secret"`
	AccessTokenTTLSeconds int           `mapstructure:"access_token_ttl_seconds"`
	RefreshTokenTTL       time.Duration `mapstructure:"refresh_token_ttl"` // 默认 30 天
	OAuth2StateTTL        time.Duration `mapstructure:"oauth2_state_ttl"`  // 默认 10 分钟
}

// ProvidersConfig OAuth2 Provider 配置集合
type ProvidersConfig struct {
	Mock        MockProviderConfig `mapstructure:"mock"`
	HTTPTimeout time.Duration      `mapstructure:"http_timeout"` // OAuth2 HTTP 客户端超时，默认 10s
}

// MockProviderConfig Mock OAuth2 Provider 配置
type MockProviderConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	URL          string `mapstructure:"url"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
}

// CORSConfig 跨域配置
type CORSConfig struct {
	// AllowOrigins 允许的 origin 列表，如 ["https://app.example.com"]
	// 为空时回退到 "*"（仅限开发环境，生产环境必须显式配置）
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

	// SummarizePrompt 上下文压缩时的摘要提示词（可选）
	// 为空时使用默认英文提示词
	SummarizePrompt string `mapstructure:"summarize_prompt"`

	// ContextTokensLimit 触发上下文压缩的 token 阈值（可选）
	// 默认 25000（约为模型上下文窗口的 20%）
	ContextTokensLimit int `mapstructure:"context_tokens_limit"`

	// CheckpointTTL eino checkpoint 在 Redis 中的存活时间
	// 默认 24h。较长的 TTL 提高崩溃恢复窗口，但增加 Redis 内存压力
	CheckpointTTL time.Duration `mapstructure:"checkpoint_ttl"`

	// StreamChunkTTL 流式消息 chunk 在 Redis 中的存活时间
	// 默认 5m。chunks 是短生命周期数据，生成完成后即删除
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
}

// APIConfig API 层配置（分页、限流等）
type APIConfig struct {
	// QueryDefaultLimit 分页查询默认每页条数，默认 50
	QueryDefaultLimit int `mapstructure:"query_default_limit"`

	// QueryMaxLimit 分页查询最大每页条数，默认 100
	QueryMaxLimit int `mapstructure:"query_max_limit"`
}

// Load 加载配置。使用局部 viper 实例，不污染全局状态，可安全并行测试。
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

	// 默认值（必须在 ReadInConfig 之前设置）
	v.SetDefault("auth.access_token_ttl_seconds", 3600)
	v.SetDefault("auth.refresh_token_ttl", 30*24*time.Hour)
	v.SetDefault("auth.oauth2_state_ttl", 10*time.Minute)
	v.SetDefault("providers.mock.enabled", true)
	v.SetDefault("providers.mock.url", "http://localhost:10060")
	v.SetDefault("providers.mock.client_id", "test-client")
	v.SetDefault("providers.mock.client_secret", "test-client-secret")
	v.SetDefault("providers.http_timeout", 10*time.Second)
	v.SetDefault("cors.allow_origins", []string{})
	v.SetDefault("server.shutdown_timeout", 10*time.Second)
	v.SetDefault("server.rpc_timeout", 10*time.Second)
	v.SetDefault("worker.heartbeat_sec", 10)
	v.SetDefault("worker.ttl_sec", 60)
	v.SetDefault("worker.idle_timeout", 5*time.Minute)
	v.SetDefault("worker.stream_block", 500*time.Millisecond)
	v.SetDefault("worker.max_len_approx", 10000)
	v.SetDefault("worker.background_concurrency", 5)
	v.SetDefault("worker.context_tokens_limit", 25000)
	v.SetDefault("worker.checkpoint_ttl", 24*time.Hour)
	v.SetDefault("worker.stream_chunk_ttl", 5*time.Minute)
	v.SetDefault("worker.interrupt_answer_ttl", 10*time.Minute)
	v.SetDefault("worker.orphan_trigger_ttl", 24*time.Hour)
	v.SetDefault("worker.lock_ttl_sec", 120)
	v.SetDefault("llm.thinking_budget_tokens", 50000)
	v.SetDefault("llm.reasoning_effort", "medium")
	v.SetDefault("api.query_default_limit", 50)
	v.SetDefault("api.query_max_limit", 100)

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
