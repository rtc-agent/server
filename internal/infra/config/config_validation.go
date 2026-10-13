package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Validate checks required configuration fields, returning the first error found.
func (c *Config) Validate() error {
	// Validate server environment.
	if err := c.validateServerEnv(); err != nil {
		return err
	}

	// Validate database configuration.
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}

	// Validate Redis configuration.
	if err := c.validateRedis(); err != nil {
		return err
	}

	// Validate server port range.
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be 1-65535, got %d", c.Server.Port)
	}

	// Validate LLM configuration.
	if err := c.validateLLM(); err != nil {
		return err
	}

	// Validate OAuth providers.
	if err := c.validateOAuthProviders(); err != nil {
		return err
	}

	// Validate auth configuration.
	if err := c.validateAuth(); err != nil {
		return err
	}

	// Validate CORS configuration.
	if err := c.validateCORS(); err != nil {
		return err
	}

	// Validate worker compression threshold configuration.
	if err := c.Worker.Validate(); err != nil {
		return err
	}

	// Validate cross-config constraints: max_output_tokens_for_summary should not exceed llm.max_tokens.
	if c.Worker.MaxOutputTokensForSummary > 0 && c.LLM.MaxTokens > 0 &&
		c.Worker.MaxOutputTokensForSummary > c.LLM.MaxTokens {
		return fmt.Errorf("worker.max_output_tokens_for_summary (%d) should be <= llm.max_tokens (%d)",
			c.Worker.MaxOutputTokensForSummary, c.LLM.MaxTokens)
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

	// Validate token exchange configuration (only when external issuers are configured).
	// Pass server environment so production deployments can enforce audience validation.
	if err := c.TokenExchange.Validate(c.Server.Env); err != nil {
		return err
	}

	return nil
}

// validateServerEnv checks that server environment is valid.
func (c *Config) validateServerEnv() error {
	switch c.Server.Env {
	case "development", "production":
		return nil
	default:
		return fmt.Errorf("server.env must be 'development' or 'production', got %q", c.Server.Env)
	}
}

// IsDevBypassAllowed returns true if JWT dev bypass (X-User-ID/X-Device-ID headers)
// should be permitted. This is only allowed in development environment.
//
// SECURITY: This method centralizes the dev bypass decision to prevent accidental
// enablement in production. It performs a defensive check against the environment
// even if the caller has already checked cfg.Server.Env == "development".
//
// Dev bypass allows requests to skip JWT validation by providing user identity
// via HTTP headers. This is useful for local development and testing, but must
// NEVER be enabled in production or staging environments.
func (c *Config) IsDevBypassAllowed() bool {
	// Defense-in-depth: explicitly check for production and staging environments
	// to prevent dev bypass from being accidentally enabled.
	switch c.Server.Env {
	case "production", "staging", "prod":
		// Never allow dev bypass in production-like environments
		return false
	case "development", "dev":
		// Only allow in explicit development environment
		return true
	default:
		// Unknown environments should default to secure (no bypass)
		return false
	}
}

// ValidateDevBypassSafety performs additional safety checks for dev bypass configuration.
// This is called during startup to provide early warning if dev bypass might be
// accidentally enabled in production-like environments.
//
// Returns a warning string if dev bypass is enabled in a non-development environment,
// or empty string if configuration is safe.
func (c *Config) ValidateDevBypassSafety() string {
	// Check if we're in a production-like environment but dev bypass would be allowed
	// by a naive environment check (e.g., if someone adds a new environment value)
	if c.Server.Env != "development" && c.Server.Env != "dev" {
		// We're in production/staging/etc - dev bypass should not be allowed
		// This is a defensive check; IsDevBypassAllowed() already handles this
		if c.IsDevBypassAllowed() {
			return "CRITICAL: IsDevBypassAllowed() returned true in non-development environment"
		}
	}
	return ""
}

// validateLLM checks LLM configuration fields.
func (c *Config) validateLLM() error {
	if c.LLM.APIKey == "" {
		return fmt.Errorf("llm.api_key is required: set it via LLM__API_KEY environment variable")
	}
	// Validate reasoning_effort if set.
	switch c.LLM.ReasoningEffort {
	case "", "low", "medium", "high":
		return nil
	default:
		return fmt.Errorf("llm.reasoning_effort (%q) must be \"low\", \"medium\", or \"high\"", c.LLM.ReasoningEffort)
	}
}

// validateOAuthProviders checks OAuth provider configuration.
func (c *Config) validateOAuthProviders() error {
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

	// SECURITY: Validate that enabled providers have required credentials.
	// Client secrets must be configured for OAuth2 flows to work.
	if c.Providers.GitHub.Enabled {
		if c.Providers.GitHub.ClientID == "" {
			return fmt.Errorf("providers.github.client_id is required when github provider is enabled")
		}
		if c.Providers.GitHub.ClientSecret == "" {
			return fmt.Errorf("providers.github.client_secret is required when github provider is enabled")
		}
	}
	if c.Providers.Google.Enabled {
		if c.Providers.Google.ClientID == "" {
			return fmt.Errorf("providers.google.client_id is required when google provider is enabled")
		}
		if c.Providers.Google.ClientSecret == "" {
			return fmt.Errorf("providers.google.client_secret is required when google provider is enabled")
		}
	}
	if c.Providers.Mock.Enabled {
		if c.Providers.Mock.ClientID == "" {
			return fmt.Errorf("providers.mock.client_id is required when mock provider is enabled")
		}
		if c.Providers.Mock.ClientSecret == "" {
			return fmt.Errorf("providers.mock.client_secret is required when mock provider is enabled")
		}
	}

	return nil
}

// validateAuth checks authentication configuration.
func (c *Config) validateAuth() error {
	if c.Auth.JWTSecret == "" {
		return fmt.Errorf("auth.jwt_secret is required")
	}
	// Security: reject weak/default JWT secrets in production.
	const weakJWTSecrets = "rtc-agent-dev-jwt-secret-change-me-in-production"
	if c.Server.Env == "production" && c.Auth.JWTSecret == weakJWTSecrets {
		return fmt.Errorf("auth.jwt_secret must not use the default development value in production: generate a secure random string")
	}
	// Security: enforce minimum JWT secret length (32 bytes for HMAC-SHA256)
	if len(c.Auth.JWTSecret) < 32 {
		return fmt.Errorf("auth.jwt_secret must be at least 32 bytes, got %d", len(c.Auth.JWTSecret))
	}
	if c.Auth.AccessTokenTTLSeconds <= 0 {
		return fmt.Errorf("auth.access_token_ttl_seconds must be positive, got %d", c.Auth.AccessTokenTTLSeconds)
	}
	// Security: enforce redirect URI whitelist in production to prevent open redirect attacks.
	if c.Server.Env == "production" && len(c.Auth.AllowedRedirectURIs) == 0 {
		return fmt.Errorf("auth.allowed_redirect_uris is required in production environment")
	}
	// Security: validate OAuth2 state TTL to prevent CSRF protection bypass.
	// TTL must be between 5 and 30 minutes to balance security and usability.
	// Too short: legitimate users may fail; too long: increases CSRF attack window.
	if c.Auth.OAuth2StateTTL > 0 {
		if c.Auth.OAuth2StateTTL < 5*time.Minute || c.Auth.OAuth2StateTTL > 30*time.Minute {
			return fmt.Errorf("auth.oauth2_state_ttl must be between 5 and 30 minutes, got %v", c.Auth.OAuth2StateTTL)
		}
	}
	return nil
}

// validateRedis checks Redis configuration.
func (c *Config) validateRedis() error {
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	// Security: warn about Redis without password in production (non-blocking)
	// This is logged as a warning but doesn't fail validation to allow legacy setups
	if c.Server.Env == "production" && c.Redis.Password == "" {
		// Log warning via stderr since logger may not be initialized
		fmt.Fprintf(os.Stderr, "[config] WARNING: redis.password is empty in production environment - consider setting a password for security\n")
	}
	return nil
}

// validateCORS checks CORS configuration.
func (c *Config) validateCORS() error {
	// Security: In production, require explicit CORS origin configuration
	// Empty AllowOrigins with development fallback to "*" is blocked by validateServerEnv
	if c.Server.Env == "production" && len(c.CORS.AllowOrigins) == 0 {
		// Log warning but don't fail - some production deployments may not need CORS
		fmt.Fprintf(os.Stderr, "[config] WARNING: cors.allow_origins is empty in production - cross-origin requests will be rejected\n")
	}
	// Validate CORS origins format
	for i, origin := range c.CORS.AllowOrigins {
		if origin == "*" {
			// Security: wildcard origin combined with credentials allows any site to make
			// authenticated cross-origin requests. Block it in production.
			if c.Server.Env == "production" {
				return fmt.Errorf("cors.allow_origins must not contain \"*\" in production environment: specify explicit trusted origins")
			}
			continue
		}
		// Security: localhost wildcard pattern (e.g., "http://localhost:*") is convenient for
		// development but poses a security risk in production/staging by allowing any localhost
		// port to make cross-origin requests. Block it in non-development environments.
		if strings.Contains(origin, "localhost:*") {
			if c.Server.Env != "development" && c.Server.Env != "dev" {
				return fmt.Errorf("cors.allow_origins must not contain \"localhost:*\" pattern in %s environment: specify explicit localhost ports (e.g., http://localhost:3000)", c.Server.Env)
			}
			continue
		}
		if _, err := url.Parse(origin); err != nil {
			return fmt.Errorf("cors.allow_origins[%d] is invalid: %w", i, err)
		}
	}
	return nil
}

// Validate checks TokenExchangeConfig fields.
// Each external issuer must have a valid name, issuer, and JWKS URI.
// JWKS URIs must use HTTPS in production to prevent cache poisoning.
// SECURITY: In production, ExpectedAudience must be configured for each issuer to prevent
// cross-service token misuse — a JWT intended for another service could otherwise be
// exchanged for an RTC token.
func (c *TokenExchangeConfig) Validate(serverEnv string) error {
	for i, iss := range c.ExternalIssuers {
		if iss.Name == "" {
			return fmt.Errorf("token_exchange.external_issuers[%d].name is required", i)
		}
		if iss.Issuer == "" {
			return fmt.Errorf("token_exchange.external_issuers[%d].issuer is required", i)
		}
		if iss.JWKSURI == "" {
			return fmt.Errorf("token_exchange.external_issuers[%d].jwks_uri is required", i)
		}
		u, err := url.Parse(iss.JWKSURI)
		if err != nil {
			return fmt.Errorf("token_exchange.external_issuers[%d].jwks_uri is invalid: %w", i, err)
		}
		// Security: require HTTPS for JWKS endpoints in production to prevent MITM cache poisoning.
		// Allow HTTP in development environment for:
		// - localhost, 127.0.0.1
		// - .local, .orb.local domains
		// - Docker internal service names (no dots in hostname)
		if u.Scheme != "https" {
			host := u.Hostname()
			isLocal := host == "localhost" || host == "127.0.0.1" ||
				strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".orb.local")
			// Allow hostnames without dots (Docker internal services like "admin-server")
			hasNoDots := !strings.Contains(host, ".")
			if !isLocal && !hasNoDots {
				return fmt.Errorf("token_exchange.external_issuers[%d].jwks_uri must use HTTPS for external hosts, got %q", i, u.Scheme)
			}
		}
		// SECURITY: Enforce audience configuration in production.
		// Without ExpectedAudience, any JWT from this issuer (even one minted for a different
		// service) can be exchanged for an RTC token — a classic confused deputy attack.
		// The runtime check in usecase.validateAudienceConfiguration provides defense-in-depth,
		// but startup validation catches misconfigurations before traffic is served.
		if serverEnv == "production" && iss.ExpectedAudience == "" {
			return fmt.Errorf("token_exchange.external_issuers[%d].expected_audience is required in production environment (issuer %q): set it to restrict which JWTs can be exchanged", i, iss.Issuer)
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
