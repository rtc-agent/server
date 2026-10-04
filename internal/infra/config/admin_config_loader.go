package config

import (
	"fmt"
	"os"

	"github.com/spf13/viper"
)

// LoadAdminConfig loads admin-server configuration from file.
func LoadAdminConfig(cfgFile string) (*AdminConfig, error) {
	if cfgFile != "" {
		if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
			return nil, fmt.Errorf("admin config file not found: %s", cfgFile)
		}
		viper.SetConfigFile(cfgFile)
	}

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read admin config: %w", err)
	}

	var cfg AdminConfig
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal admin config: %w", err)
	}

	// Set defaults
	cfg.setDefaults()

	return &cfg, nil
}

// setDefaults sets default values for AdminConfig.
func (c *AdminConfig) setDefaults() {
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8081
	}
	if c.Server.Env == "" {
		c.Server.Env = "development"
	}
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

// Validate validates the admin configuration.
func (c *AdminConfig) Validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}

	// Validate JWT algorithm
	switch c.JWT.Algorithm {
	case "RS256", "RS384", "RS512", "ES256", "ES384", "ES512":
		// Valid algorithm
	default:
		return fmt.Errorf("jwt.algorithm must be one of: RS256, RS384, RS512, ES256, ES384, ES512")
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

	// Note: PrivateKeyPath and PublicKeyPath are optional (ephemeral keys generated if not provided)
	// If one is provided, both must be provided
	if (c.JWT.PrivateKeyPath == "") != (c.JWT.PublicKeyPath == "") {
		return fmt.Errorf("jwt.private_key_path and jwt.public_key_path must both be set or both be empty")
	}

	return nil
}
