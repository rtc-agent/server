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
	// Email configuration for sending verification codes and notifications
	Email AdminEmailConfig `mapstructure:"email"`
	// OTP configuration for email verification code settings
	OTP AdminOTPConfig `mapstructure:"otp"`
	// PrometheusURL is the base URL of the Prometheus instance for metrics proxying.
	// If empty, the metrics proxy endpoint returns 501 Not Implemented.
	PrometheusURL string `mapstructure:"prometheus_url"`
	// GrafanaURL is the base URL of the Grafana instance for dashboard iframe proxying.
	// If empty, the grafana proxy endpoint returns 501 Not Implemented.
	GrafanaURL string `mapstructure:"grafana_url"`
	// JaegerURL is the base URL of the Jaeger instance for tracing proxying.
	// If empty, the jaeger proxy endpoint returns 501 Not Implemented.
	JaegerURL string `mapstructure:"jaeger_url"`
	// PyroscopeURL is the base URL of the Pyroscope instance for profiling proxying.
	// If empty, the pyroscope proxy endpoint returns 501 Not Implemented.
	PyroscopeURL string `mapstructure:"pyroscope_url"`
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
	// PasswordEnabled enables password-based login.
	// When false, only OTP login is available.
	// Default: false for security; set to true in development environments.
	PasswordEnabled bool `mapstructure:"password_enabled"`
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

// AdminEmailConfig SMTP email settings for admin-server.
// Used for sending verification codes and other notifications.
type AdminEmailConfig struct {
	// SMTPHost SMTP server host (e.g. "smtp.gmail.com")
	SMTPHost string `mapstructure:"smtp_host"`
	// SMTPPort SMTP server port (e.g. 587 for STARTTLS)
	SMTPPort int `mapstructure:"smtp_port"`
	// SMTPUser SMTP authentication username
	SMTPUser string `mapstructure:"smtp_user"`
	// SMTPPassword SMTP authentication password. Supports ${ENV_VAR} syntax.
	SMTPPassword string `mapstructure:"smtp_password"`
	// FromAddress sender email address (e.g. "noreply@example.com")
	FromAddress string `mapstructure:"from_address"`
	// FromName sender display name (e.g. "RTC Agent")
	FromName string `mapstructure:"from_name"`
}

// AdminOTPConfig settings for email verification code (OTP).
type AdminOTPConfig struct {
	// TTL verification code validity period in seconds. Default: 300 (5 minutes).
	TTL int `mapstructure:"ttl"`
	// Length verification code length. Default: 6.
	Length int `mapstructure:"length"`
	// SendCooldown cooldown period in seconds between sending codes to the same email. Default: 60.
	SendCooldown int `mapstructure:"send_cooldown"`
	// MaxSendPerIP max OTP send requests per IP per minute. Default: 5.
	MaxSendPerIP int `mapstructure:"max_send_per_ip"`
	// MaxVerifyAttempts max failed verification attempts before lockout. Default: 5.
	MaxVerifyAttempts int `mapstructure:"max_verify_attempts"`
	// LockDuration lockout duration in seconds after exceeding max verify attempts. Default: 900 (15 minutes).
	LockDuration int `mapstructure:"lock_duration"`
}
