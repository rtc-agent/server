package config

import (
	"fmt"
	"net/url"
)

// Validate checks required configuration fields, returning the first error found.
func (c *Config) Validate() error {
	// Validate server environment.
	switch c.Server.Env {
	case "development", "production":
		// valid
	default:
		return fmt.Errorf("server.env must be 'development' or 'production', got %q", c.Server.Env)
	}

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
	// Validate reasoning_effort if set.
	switch c.LLM.ReasoningEffort {
	case "", "low", "medium", "high":
		// valid
	default:
		return fmt.Errorf("llm.reasoning_effort (%q) must be \"low\", \"medium\", or \"high\"", c.LLM.ReasoningEffort)
	}

	// Validate that at least one OAuth provider is enabled.
	if !c.Providers.Mock.Enabled && !c.Providers.GitHub.Enabled && !c.Providers.Google.Enabled {
		return fmt.Errorf("at least one OAuth provider must be enabled (mock, github, or google)")
	}

	// Mock OAuth provider must not be enabled in production.
	if c.Server.Env == "production" && c.Providers.Mock.Enabled {
		return fmt.Errorf("providers.mock.enabled must be false in production environment")
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
	// Security: reject weak/default JWT secrets in production.
	const weakJWTSecrets = "rtc-agent-dev-jwt-secret-change-me-in-production"
	if c.Auth.JWTSecret == weakJWTSecrets {
		return fmt.Errorf("auth.jwt_secret must not use the default development value in production: generate a secure random string")
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

	// Validate storage configuration (only when storage is enabled).
	if c.Storage.IsEnabled() {
		// Validate storage encryption configuration.
		if err := c.Storage.Encryption.Validate(); err != nil {
			return err
		}
		if err := c.Storage.Quota.Validate(); err != nil {
			return err
		}
		if err := c.Storage.RateLimit.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// Validate checks EncryptionConfig fields.
// SessionTokenKey must be a secure random value, not empty or the default test value.
func (c *EncryptionConfig) Validate() error {
	if c.SessionTokenKey == "" {
		return fmt.Errorf("storage.encryption.session_token_key is required: set it via STORAGE__ENCRYPTION__SESSION_TOKEN_KEY environment variable")
	}

	// Check for the default test value from config.docker.yaml
	const defaultTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if c.SessionTokenKey == defaultTestKey {
		return fmt.Errorf("storage.encryption.session_token_key must not use the default test value in production: generate a secure random 64-character hex string")
	}

	// Validate length (64 hex chars = 32 bytes for AES-256)
	if len(c.SessionTokenKey) != 64 {
		return fmt.Errorf("storage.encryption.session_token_key must be 64 hex characters (32 bytes), got %d characters", len(c.SessionTokenKey))
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

// Validate checks QuotaConfig fields.
// All quota limits must be positive to ensure meaningful constraints.
func (c *QuotaConfig) Validate() error {
	if c.MaxFileSizeBytes <= 0 {
		return fmt.Errorf("storage.quota.max_file_size_bytes must be positive, got %d", c.MaxFileSizeBytes)
	}
	if c.MaxUserQuotaBytes <= 0 {
		return fmt.Errorf("storage.quota.max_user_quota_bytes must be positive, got %d", c.MaxUserQuotaBytes)
	}
	if c.MaxConcurrentUploads <= 0 {
		return fmt.Errorf("storage.quota.max_concurrent_uploads must be positive, got %d", c.MaxConcurrentUploads)
	}
	if c.PendingTTL <= 0 {
		return fmt.Errorf("storage.quota.pending_ttl must be positive, got %v", c.PendingTTL)
	}
	return nil
}

// Validate checks RateLimitConfig fields.
// Rate limit must be positive to allow requests.
func (c *RateLimitConfig) Validate() error {
	if c.RequestsPerMinute <= 0 {
		return fmt.Errorf("storage.rate_limit.requests_per_minute must be positive, got %d", c.RequestsPerMinute)
	}
	return nil
}
