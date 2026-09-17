package config

import (
	"os"
	"regexp"
)

// expandEnvVars expands environment variable references in sensitive configuration fields.
// Only applies to fields containing sensitive information (passwords, secrets, DSNs).
func expandEnvVars(cfg *Config) {
	// Database connection string (may contain password).
	cfg.Database.DSN = expandEnvRef(cfg.Database.DSN)

	// Redis password.
	cfg.Redis.Password = expandEnvRef(cfg.Redis.Password)

	// Asynq Redis password.
	cfg.Asynq.RedisPassword = expandEnvRef(cfg.Asynq.RedisPassword)

	// LLM API key.
	cfg.LLM.APIKey = expandEnvRef(cfg.LLM.APIKey)

	// OAuth2 provider secrets.
	cfg.Providers.Mock.ClientSecret = expandEnvRef(cfg.Providers.Mock.ClientSecret)
	cfg.Providers.GitHub.ClientSecret = expandEnvRef(cfg.Providers.GitHub.ClientSecret)
	cfg.Providers.Google.ClientSecret = expandEnvRef(cfg.Providers.Google.ClientSecret)

	// Auth secrets.
	cfg.Auth.JWTSecret = expandEnvRef(cfg.Auth.JWTSecret)

	// Metrics password.
	cfg.Metrics.Password = expandEnvRef(cfg.Metrics.Password)
	cfg.Debug.Password = expandEnvRef(cfg.Debug.Password)
}

// envRefPattern matches environment variable references in ${VAR_NAME} form.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRef replaces ${VAR} references in a string with the corresponding environment variable values.
// If the environment variable is not set, it is replaced with an empty string. Strings without ${...} are returned unchanged.
func expandEnvRef(s string) string {
	return envRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		key := envRefPattern.FindStringSubmatch(match)[1]
		val, ok := os.LookupEnv(key)
		if !ok {
			return ""
		}
		return val
	})
}

// ExpandEnvRef is exported for testing.
func ExpandEnvRef(s string) string {
	return expandEnvRef(s)
}
