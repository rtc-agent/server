package config

import (
	"fmt"
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

	// Web search sensitive fields.
	cfg.WebSearch.Redis.Password = expandEnvRef(cfg.WebSearch.Redis.Password)
	for i := range cfg.WebSearch.Providers {
		cfg.WebSearch.Providers[i].APIKey = expandEnvRef(cfg.WebSearch.Providers[i].APIKey)
	}
	for i := range cfg.WebSearch.Proxies {
		cfg.WebSearch.Proxies[i].URL = expandEnvRef(cfg.WebSearch.Proxies[i].URL)
	}

	// Web fetch sensitive fields.
	cfg.WebFetch.Redis.Password = expandEnvRef(cfg.WebFetch.Redis.Password)

	// Storage (rtc-oss3) sensitive fields.
	cfg.Storage.MinIO.AccessKey = expandEnvRef(cfg.Storage.MinIO.AccessKey)
	cfg.Storage.MinIO.SecretKey = expandEnvRef(cfg.Storage.MinIO.SecretKey)
	cfg.Storage.Encryption.SessionTokenKey = expandEnvRef(cfg.Storage.Encryption.SessionTokenKey)
	cfg.Storage.S3Endpoint.TLSCert = expandEnvRef(cfg.Storage.S3Endpoint.TLSCert)
	cfg.Storage.S3Endpoint.TLSKey = expandEnvRef(cfg.Storage.S3Endpoint.TLSKey)
}

// envRefPattern matches environment variable references in ${VAR_NAME} form.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRef replaces ${VAR} references in a string with the corresponding environment variable values.
// If the environment variable is not set, a warning is logged and the reference is replaced with an empty string.
// Strings without ${...} are returned unchanged.
func expandEnvRef(s string) string {
	return envRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		key := envRefPattern.FindStringSubmatch(match)[1]
		val, ok := os.LookupEnv(key)
		if !ok {
			// Log warning for unset environment variable references to aid debugging.
			// Use fmt.Fprintf to stderr since logger may not be initialized yet.
			fmt.Fprintf(os.Stderr, "[config] warning: environment variable %q is not set, using empty string\n", key)
			return ""
		}
		return val
	})
}

// ExpandEnvRef is exported for testing.
func ExpandEnvRef(s string) string {
	return expandEnvRef(s)
}
