package config

// AdminConfig admin-server configuration
type AdminConfig struct {
	// Server configuration for the admin HTTP server
	Server AdminServerConfig `mapstructure:"server"`
	// Database configuration, reuses the shared DatabaseConfig
	Database DatabaseConfig `mapstructure:"database"`
	// JWT configuration for admin authentication tokens
	JWT AdminJWTConfig `mapstructure:"jwt"`
}

// AdminServerConfig HTTP server binding configuration for admin-server
type AdminServerConfig struct {
	// Host address to bind the admin HTTP server
	Host string `mapstructure:"host"`
	// Port number to listen on
	Port int `mapstructure:"port"`
	// Environment name (development, staging, production)
	Env string `mapstructure:"env"`
}

// AdminJWTConfig JWT token generation and validation settings for admin-server
type AdminJWTConfig struct {
	// Signing algorithm (RS256 or ES256)
	Algorithm string `mapstructure:"algorithm"`
	// Issuer claim (iss) embedded in generated JWTs
	Issuer string `mapstructure:"issuer"`
	// Audience claim (aud) embedded in generated JWTs
	Audience string `mapstructure:"audience"`
	// PrivateKeyPath path to the PEM-encoded private key used for signing
	PrivateKeyPath string `mapstructure:"private_key_path"`
	// PublicKeyPath path to the PEM-encoded public key used for verification
	PublicKeyPath string `mapstructure:"public_key_path"`
	// AccessTokenTTL access token lifetime in seconds
	AccessTokenTTL int `mapstructure:"access_token_ttl"`
	// RefreshTokenTTL refresh token lifetime in seconds
	RefreshTokenTTL int `mapstructure:"refresh_token_ttl"`
}
