package config

// AdminConfig admin-server configuration
type AdminConfig struct {
	// Server configuration for the admin HTTP server
	Server AdminServerConfig `mapstructure:"server"`
	// Database configuration, reuses the shared DatabaseConfig
	Database DatabaseConfig `mapstructure:"database"`
	// Redis configuration for distributed caching (JWKS, rate limiting).
	// Optional in development; required in production for multi-instance deployments.
	Redis AdminRedisConfig `mapstructure:"redis"`
	// JWT configuration for admin authentication tokens
	JWT AdminJWTConfig `mapstructure:"jwt"`
	// CORS configuration for admin-server
	CORS AdminCORSConfig `mapstructure:"cors"`
	// Security configuration for rate limiting and login protection
	Security AdminSecurityConfig `mapstructure:"security"`
	// Features configuration for feature flags
	Features AdminFeaturesConfig `mapstructure:"features"`
}

// AdminSecurityConfig security settings for admin-server.
type AdminSecurityConfig struct {
	// RateLimitPerMinute is the maximum number of requests allowed per minute per IP.
	// Defaults to 100 if not set.
	RateLimitPerMinute int `mapstructure:"rate_limit_per_minute"`
	// LoginMaxAttempts is the maximum number of failed login attempts before lockout.
	// Applies to both IP and email independently. Defaults to 5.
	LoginMaxAttempts int `mapstructure:"login_max_attempts"`
	// LoginLockDuration is the lockout duration in seconds after exceeding max attempts.
	// Defaults to 900 (15 minutes).
	LoginLockDuration int `mapstructure:"login_lock_duration"`
}

// AdminFeaturesConfig controls feature flags for the admin server.
type AdminFeaturesConfig struct {
	// PermissionSystem enables the Casbin RBAC permission system.
	// When false, all authenticated users have full admin access (legacy behavior).
	// Default: true (per spec section 8.2)
	PermissionSystem bool `mapstructure:"permission_system"`
}

// AdminRedisConfig Redis connection settings for admin-server
type AdminRedisConfig struct {
	// Addr Redis server address (e.g. "localhost:6379" or "redis.internal:6379")
	Addr string `mapstructure:"addr"`
	// Password Redis authentication password. Empty means no authentication.
	Password string `mapstructure:"password"`
	// DB Redis database number (0-15). Defaults to 0.
	DB int `mapstructure:"db"`
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

// AdminCORSConfig CORS settings for admin-server
type AdminCORSConfig struct {
	// AllowedOrigins is the list of origins allowed for CORS.
	// Empty means all origins are allowed (development only).
	// Production deployments MUST specify explicit origins.
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}
