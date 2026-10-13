package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// LoadAdminConfig loads admin-server configuration from file.
func LoadAdminConfig(cfgFile string) (*AdminConfig, error) {
	v := viper.New()
	if cfgFile != "" {
		if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
			return nil, fmt.Errorf("admin config file not found: %s", cfgFile)
		}
		v.SetConfigFile(cfgFile)
	}

	// Support environment variable overrides: EMAIL__SMTP_HOST → email.smtp_host
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "__"))

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read admin config: %w", err)
	}

	var cfg AdminConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal admin config: %w", err)
	}

	// Set defaults
	cfg.setDefaults(v)

	// Expand environment variable references in sensitive fields (${VAR_NAME} form)
	expandAdminEnvVars(&cfg)

	return &cfg, nil
}

// setDefaults sets default values for AdminConfig.
func (c *AdminConfig) setDefaults(v *viper.Viper) {
	c.setServerDefaults()
	c.setJWTDefaults()
	c.setSecurityDefaults(v)
	c.setEmailDefaults()
	c.setOTPDefaults()
	c.setCSPDefaults(v)
}

// setServerDefaults sets default values for server configuration.
func (c *AdminConfig) setServerDefaults() {
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8081
	}
	if c.Server.Env == "" {
		c.Server.Env = "development"
	}
}

// setJWTDefaults sets default values for JWT configuration.
func (c *AdminConfig) setJWTDefaults() {
	if c.JWT.Algorithm == "" {
		c.JWT.Algorithm = "RS256"
	}
	if c.JWT.Issuer == "" {
		c.JWT.Issuer = "https://admin.example.com"
	}
	if c.JWT.Audience == "" {
		c.JWT.Audience = "https://rtc.example.com"
	}
	if c.JWT.AccessTokenTTL == 0 {
		c.JWT.AccessTokenTTL = 3600 // 1 hour
	}
	if c.JWT.RefreshTokenTTL == 0 {
		c.JWT.RefreshTokenTTL = 604800 // 7 days
	}
}

// setSecurityDefaults sets default values for security configuration.
func (c *AdminConfig) setSecurityDefaults(v *viper.Viper) {
	// Rate limiting and login protection defaults
	if c.Security.RateLimitPerMinute <= 0 {
		c.Security.RateLimitPerMinute = 100
	}
	if c.Security.LoginMaxAttempts <= 0 {
		c.Security.LoginMaxAttempts = 5
	}
	if c.Security.LoginLockDuration <= 0 {
		c.Security.LoginLockDuration = 900 // 15 minutes
	}
	// LoginProtectionBackend defaults to "auto" (use Redis if available).
	// Only set default if not explicitly configured in the config file.
	if !v.IsSet("security.login_protection_backend") {
		c.Security.LoginProtectionBackend = "auto"
	}
	// CookieSecure defaults to true in production, false in development.
	// Only set default if not explicitly configured in the config file.
	if !v.IsSet("security.cookie_secure") {
		c.Security.CookieSecure = c.Server.Env == "production"
	}
	// Features: permission_system defaults to true per spec section 8.2.
	// Only set default if not explicitly configured in the config file.
	if !v.IsSet("features.permission_system") {
		c.Features.PermissionSystem = true
	}
	// Features: password_enabled defaults to false for security.
	// Only set default if not explicitly configured in the config file or environment.
	if !v.IsSet("features.password_enabled") {
		c.Features.PasswordEnabled = false
	}
}

// setEmailDefaults sets default values for email configuration.
func (c *AdminConfig) setEmailDefaults() {
	if c.Email.SMTPPort == 0 {
		c.Email.SMTPPort = 587
	}
	if c.Email.FromName == "" {
		c.Email.FromName = "RTC Agent"
	}
}

// setOTPDefaults sets default values for OTP configuration.
func (c *AdminConfig) setOTPDefaults() {
	if c.OTP.TTL == 0 {
		c.OTP.TTL = 300 // 5 minutes
	}
	if c.OTP.Length == 0 {
		c.OTP.Length = 6
	}
	if c.OTP.SendCooldown == 0 {
		c.OTP.SendCooldown = 60 // 1 minute
	}
	if c.OTP.MaxSendPerIP == 0 {
		c.OTP.MaxSendPerIP = 5
	}
	if c.OTP.MaxVerifyAttempts == 0 {
		c.OTP.MaxVerifyAttempts = 5
	}
	if c.OTP.LockDuration == 0 {
		c.OTP.LockDuration = 900 // 15 minutes
	}
}

// setCSPDefaults sets default values for Content Security Policy configuration.
func (c *AdminConfig) setCSPDefaults(v *viper.Viper) {
	// Enable CSP by default for admin server (security best practice)
	if !v.IsSet("security.csp.enabled") {
		c.Security.CSP.Enabled = true
	}
	if !v.IsSet("security.csp.report_only") {
		c.Security.CSP.ReportOnly = true // Report-only by default for development
	}
	if c.Security.CSP.ReportURI == "" {
		c.Security.CSP.ReportURI = "/api/csp-report"
	}
	if len(c.Security.CSP.DefaultSrc) == 0 {
		c.Security.CSP.DefaultSrc = []string{"'self'"}
	}
	if len(c.Security.CSP.FrameAncestors) == 0 {
		c.Security.CSP.FrameAncestors = []string{"'none'"}
	}
	if len(c.Security.CSP.BaseURI) == 0 {
		c.Security.CSP.BaseURI = []string{"'self'"}
	}
	if len(c.Security.CSP.FormAction) == 0 {
		c.Security.CSP.FormAction = []string{"'self'"}
	}
}

// Validate validates the admin configuration.
func (c *AdminConfig) Validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}

	// Validate JWT algorithm
	switch c.JWT.Algorithm {
	case "RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA":
		// Valid algorithm
	default:
		return fmt.Errorf("jwt.algorithm must be one of: RS256, RS384, RS512, ES256, ES384, ES512, EdDSA")
	}

	// Validate JWT TTL
	if c.JWT.AccessTokenTTL <= 0 {
		return fmt.Errorf("jwt.access_token_ttl must be positive")
	}
	if c.JWT.RefreshTokenTTL <= 0 {
		return fmt.Errorf("jwt.refresh_token_ttl must be positive")
	}

	// Validate server config
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}

	// Validate login protection backend
	switch c.Security.LoginProtectionBackend {
	case "auto", "redis", "memory":
		// Valid backend
	default:
		return fmt.Errorf("security.login_protection_backend must be one of: auto, redis, memory")
	}

	// Note: PrivateKeyPath and PublicKeyPath are optional (ephemeral keys generated if not provided)
	// If one is provided, both must be provided
	if (c.JWT.PrivateKeyPath == "") != (c.JWT.PublicKeyPath == "") {
		return fmt.Errorf("jwt.private_key_path and jwt.public_key_path must both be set or both be empty")
	}

	return nil
}
