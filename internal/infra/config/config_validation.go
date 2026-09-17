package config

import (
	"fmt"
	"net/url"
)

// Validate 校验必填配置项，返回第一个发现的错误。
func (c *Config) Validate() error {
	// 校验数据库配置
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}

	// 校验服务器端口范围
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be 1-65535, got %d", c.Server.Port)
	}

	// 校验 LLM 配置
	if c.LLM.APIKey == "" {
		return fmt.Errorf("llm.api_key is required: set it directly or via ${LLM_API_KEY} environment variable")
	}

	// 校验至少启用了一个 OAuth Provider
	if !c.Providers.Mock.Enabled && !c.Providers.GitHub.Enabled && !c.Providers.Google.Enabled {
		return fmt.Errorf("at least one OAuth provider must be enabled (mock, github, or google)")
	}

	// 校验 OAuth2 Provider URL 格式
	if c.Providers.Mock.Enabled && c.Providers.Mock.URL != "" {
		if _, err := url.Parse(c.Providers.Mock.URL); err != nil {
			return fmt.Errorf("providers.mock.url is invalid: %w", err)
		}
	}

	// 校验 Auth 配置
	if c.Auth.JWTSecret == "" {
		return fmt.Errorf("auth.jwt_secret is required")
	}
	if c.Auth.AccessTokenTTLSeconds <= 0 {
		return fmt.Errorf("auth.access_token_ttl_seconds must be positive, got %d", c.Auth.AccessTokenTTLSeconds)
	}

	// 校验 Worker 压缩阈值配置
	if err := c.Worker.Validate(); err != nil {
		return err
	}

	// 校验 Asynq 配置
	if err := c.Asynq.Validate(); err != nil {
		return err
	}

	return nil
}

// Validate 校验 AsynqConfig 配置。
func (c *AsynqConfig) Validate() error {
	if c.Concurrency < 0 {
		return fmt.Errorf("asynq.concurrency must be non-negative, got %d", c.Concurrency)
	}
	if c.RecoveryInterval < 0 {
		return fmt.Errorf("asynq.recovery_interval must be non-negative, got %v", c.RecoveryInterval)
	}
	if c.StaleThreshold < 0 {
		return fmt.Errorf("asynq.stale_threshold must be non-negative, got %v", c.StaleThreshold)
	}
	if c.RetryMax < 0 {
		return fmt.Errorf("asynq.retry_max must be non-negative, got %d", c.RetryMax)
	}
	if c.RetryTimeout < 0 {
		return fmt.Errorf("asynq.retry_timeout must be non-negative, got %v", c.RetryTimeout)
	}
	if c.HealthCheckInterval < 0 {
		return fmt.Errorf("asynq.health_check_interval must be non-negative, got %v", c.HealthCheckInterval)
	}
	return nil
}

// Validate 校验 WorkerConfig 中的压缩阈值配置。
// context_tokens_limit 必须大于 auto_compact_buffer_tokens，否则实际阈值将为非正数，
// 导致频繁触发压缩。
func (c *WorkerConfig) Validate() error {
	if c.ContextTokensLimit > 0 && c.AutoCompactBufferTokens > 0 &&
		c.ContextTokensLimit <= c.AutoCompactBufferTokens {
		return fmt.Errorf("worker.context_tokens_limit (%d) must be > worker.auto_compact_buffer_tokens (%d)",
			c.ContextTokensLimit, c.AutoCompactBufferTokens)
	}
	// 校验 TokenCounterMode 合法值
	switch c.TokenCounterMode {
	case "", "heuristic", "tokenizer":
		// valid
	default:
		return fmt.Errorf("worker.token_counter_mode (%q) must be \"heuristic\" or \"tokenizer\"", c.TokenCounterMode)
	}
	return nil
}
