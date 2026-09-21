package config

import (
	"fmt"
	"net/url"
)

// Validate checks required configuration fields, returning the first error found.
func (c *Config) Validate() error {
	// Validate database configuration.
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}

	// Validate server port range.
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be 1-65535, got %d", c.Server.Port)
	}

	// Validate LLM configuration.
	if c.LLM.APIKey == "" {
		return fmt.Errorf("llm.api_key is required: set it via LLM__API_KEY environment variable")
	}

	// Validate that at least one OAuth provider is enabled.
	if !c.Providers.Mock.Enabled && !c.Providers.GitHub.Enabled && !c.Providers.Google.Enabled {
		return fmt.Errorf("at least one OAuth provider must be enabled (mock, github, or google)")
	}

	// Validate OAuth2 provider URL format.
	if c.Providers.Mock.Enabled && c.Providers.Mock.URL != "" {
		if _, err := url.Parse(c.Providers.Mock.URL); err != nil {
			return fmt.Errorf("providers.mock.url is invalid: %w", err)
		}
	}

	// Validate auth configuration.
	if c.Auth.JWTSecret == "" {
		return fmt.Errorf("auth.jwt_secret is required")
	}
	if c.Auth.AccessTokenTTLSeconds <= 0 {
		return fmt.Errorf("auth.access_token_ttl_seconds must be positive, got %d", c.Auth.AccessTokenTTLSeconds)
	}

	// Validate worker compression threshold configuration.
	if err := c.Worker.Validate(); err != nil {
		return err
	}

	// Validate asynq configuration.
	if err := c.Asynq.Validate(); err != nil {
		return err
	}

	return nil
}

// Validate checks AsynqConfig fields.
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

// Validate checks compression threshold fields in WorkerConfig.
// context_tokens_limit must be greater than auto_compact_buffer_tokens, otherwise the actual threshold would be non-positive,
// causing frequent compression triggers.
func (c *WorkerConfig) Validate() error {
	if c.ContextTokensLimit > 0 && c.AutoCompactBufferTokens > 0 &&
		c.ContextTokensLimit <= c.AutoCompactBufferTokens {
		return fmt.Errorf("worker.context_tokens_limit (%d) must be > worker.auto_compact_buffer_tokens (%d)",
			c.ContextTokensLimit, c.AutoCompactBufferTokens)
	}
	// Validate TokenCounterMode is a valid value.
	switch c.TokenCounterMode {
	case "", "heuristic", "tokenizer":
		// valid
	default:
		return fmt.Errorf("worker.token_counter_mode (%q) must be \"heuristic\" or \"tokenizer\"", c.TokenCounterMode)
	}
	return nil
}
