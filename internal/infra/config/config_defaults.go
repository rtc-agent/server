package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Load loads configuration. Uses a local viper instance to avoid polluting global state; safe for parallel tests.
//
// Config merge strategy (when --config is not specified):
//  1. Load etc/config.yaml as baseline
//  2. If etc/config.local.yaml exists, merge and override baseline (only write differences)
//  3. config.local.yaml should be in .gitignore, used for local personal configuration
//
// When --config is specified, only loads the specified file without merging.
func Load(cfgFile string) (*Config, error) {
	v := viper.New()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.AddConfigPath("etc")
		v.SetConfigName("config")
		v.SetConfigType("yaml")
	}

	v.AutomaticEnv()
	// Environment variable mapping: DATABASE__DSN -> database.dsn, REDIS__ADDR -> redis.addr.
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "__"))

	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	// When --config is not specified, auto-merge etc/config.local.yaml (if it exists).
	if cfgFile == "" {
		localV := viper.New()
		localV.AddConfigPath("etc")
		localV.SetConfigName("config.local")
		localV.SetConfigType("yaml")
		if err := localV.ReadInConfig(); err == nil {
			for _, key := range localV.AllKeys() {
				v.Set(key, localV.Get(key))
			}
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Expand environment variable references in sensitive configuration fields (${VAR_NAME} form).
	// Only applies to sensitive fields to prevent other config items from inadvertently using environment variables.
	expandEnvVars(&cfg)

	return &cfg, nil
}

// setDefaults registers all default configuration values with viper.
// Defaults are organized by module for clarity and maintainability.
func setDefaults(v *viper.Viper) {
	// Server defaults
	setServerDefaults(v)

	// Auth defaults
	setAuthDefaults(v)

	// OAuth provider defaults
	setProviderDefaults(v)

	// CORS defaults
	setCORSDefaults(v)

	// Debug defaults
	setDebugDefaults(v)

	// Worker defaults
	setWorkerDefaults(v)

	// LLM defaults
	setLLMDefaults(v)

	// API defaults
	setAPIDefaults(v)

	// Tracing defaults
	setTracingDefaults(v)

	// Log defaults
	setLogDefaults(v)

	// Asynq defaults
	setAsynqDefaults(v)

	// Web search defaults
	setWebSearchDefaults(v)

	// Web fetch defaults
	setWebFetchDefaults(v)

	// Storage defaults
	setStorageDefaults(v)
}

func setServerDefaults(v *viper.Viper) {
	v.SetDefault("server.env", "production")
	v.SetDefault("server.shutdown_timeout", 10*time.Second)
	v.SetDefault("server.rpc_timeout", 10*time.Second)
}

func setAuthDefaults(v *viper.Viper) {
	// Security: 15-minute access token TTL reduces risk window if token is leaked.
	v.SetDefault("auth.access_token_ttl_seconds", 900)
	v.SetDefault("auth.refresh_token_ttl", 30*24*time.Hour)
	v.SetDefault("auth.oauth2_state_ttl", 10*time.Minute)
	v.SetDefault("auth.allowed_redirect_uris", []string{})
}

func setProviderDefaults(v *viper.Viper) {
	v.SetDefault("providers.mock.enabled", true)
	v.SetDefault("providers.mock.url", "http://localhost:10060")
	v.SetDefault("providers.mock.client_id", "test-client")
	v.SetDefault("providers.mock.client_secret", "test-client-secret")

	v.SetDefault("providers.github.enabled", false)
	v.SetDefault("providers.github.client_id", "")
	v.SetDefault("providers.github.client_secret", "")
	v.SetDefault("providers.github.scope", "read:user user:email")

	v.SetDefault("providers.google.enabled", false)
	v.SetDefault("providers.google.client_id", "")
	v.SetDefault("providers.google.client_secret", "")
	v.SetDefault("providers.google.scope", "openid email profile")

	v.SetDefault("providers.http_timeout", 10*time.Second)
}

func setCORSDefaults(v *viper.Viper) {
	v.SetDefault("cors.allow_origins", []string{})
}

func setDebugDefaults(v *viper.Viper) {
	// Security: debug endpoints disabled by default to prevent info leakage in production.
	// Enable explicitly via DEBUG__ENABLED=true or debug.enabled in config.yaml.
	v.SetDefault("debug.enabled", false)
	v.SetDefault("debug.goroutine_leak_threshold", 1000)
}

func setWorkerDefaults(v *viper.Viper) {
	v.SetDefault("worker.heartbeat_sec", 10)
	v.SetDefault("worker.ttl_sec", 60)
	v.SetDefault("worker.idle_timeout", 5*time.Minute)
	v.SetDefault("worker.stream_block", 500*time.Millisecond)
	v.SetDefault("worker.max_len_approx", 10000)
	v.SetDefault("worker.background_concurrency", 5)
	v.SetDefault("worker.context_tokens_limit", 25000)
	v.SetDefault("worker.auto_compact_buffer_tokens", 13000)
	v.SetDefault("worker.max_output_tokens_for_summary", 20000)
	v.SetDefault("worker.checkpoint_ttl", 24*time.Hour)
	v.SetDefault("worker.stream_chunk_ttl", 5*time.Minute)
	v.SetDefault("worker.interrupt_answer_ttl", 10*time.Minute)
	v.SetDefault("worker.orphan_trigger_ttl", 24*time.Hour)
	v.SetDefault("worker.lock_ttl_sec", 120)
	v.SetDefault("worker.token_counter_mode", "heuristic")
	v.SetDefault("worker.enable_strategic_cache_breakpoints", true)
}

func setLLMDefaults(v *viper.Viper) {
	v.SetDefault("llm.api_key", "")
	v.SetDefault("llm.thinking_budget_tokens", 50000)
	v.SetDefault("llm.reasoning_effort", "medium")
	v.SetDefault("llm.retry_max_attempts", 0)
	v.SetDefault("llm.retry_base_delay", 1*time.Second)
	// Default pricing: Claude 3.5 Sonnet (USD per million tokens).
	v.SetDefault("llm.pricing.input_per_million", 3.0)
	v.SetDefault("llm.pricing.output_per_million", 15.0)
	v.SetDefault("llm.pricing.cached_read_per_million", 0.3)
	v.SetDefault("llm.pricing.cached_write_per_million", 3.75)
	v.SetDefault("llm.pricing.reasoning_per_million", 0.0)
}

func setAPIDefaults(v *viper.Viper) {
	v.SetDefault("api.query_default_limit", 50)
	v.SetDefault("api.query_max_limit", 100)
}

func setTracingDefaults(v *viper.Viper) {
	v.SetDefault("tracing.enabled", false)
	v.SetDefault("tracing.endpoint", "localhost:4317")
	v.SetDefault("tracing.sample_rate", 1.0)
}

func setLogDefaults(v *viper.Viper) {
	v.SetDefault("log.llm_payload", false)
}

func setAsynqDefaults(v *viper.Viper) {
	v.SetDefault("asynq.concurrency", 10)
	v.SetDefault("asynq.queue", "loop")
	v.SetDefault("asynq.recovery_interval", 1*time.Minute)
	v.SetDefault("asynq.stale_threshold", 5*time.Minute)
	v.SetDefault("asynq.retry_max", 3)
	v.SetDefault("asynq.retry_timeout", 30*time.Second)
	v.SetDefault("asynq.health_check_interval", 30*time.Second)
}

func setWebSearchDefaults(v *viper.Viper) {
	v.SetDefault("web_search.enabled", false)
	v.SetDefault("web_search.balancer_type", "round_robin")
	v.SetDefault("web_search.global_timeout", 30*time.Second)
	v.SetDefault("web_search.proxy_health_url", "https://www.google.com")
	v.SetDefault("web_search.proxy_check_interval", 30*time.Second)
	v.SetDefault("web_search.circuit_breaker.failure_threshold", 50)
	v.SetDefault("web_search.circuit_breaker.open_timeout", 60*time.Second)
	v.SetDefault("web_search.circuit_breaker.half_open_max_requests", 3)
	v.SetDefault("web_search.circuit_breaker.window_size", 100)
	v.SetDefault("web_search.circuit_breaker.window_duration", 5*time.Minute)
	v.SetDefault("web_search.rate_limiter.rate", 10)
	v.SetDefault("web_search.rate_limiter.burst", 20)
	v.SetDefault("web_search.retry.max_retries", 3)
	v.SetDefault("web_search.retry.retry_delay", 1*time.Second)
	v.SetDefault("web_search.retry.backoff_factor", 2.0)
}

func setWebFetchDefaults(v *viper.Viper) {
	v.SetDefault("web_fetch.enabled", false)
	v.SetDefault("web_fetch.max_concurrency", 20)
	v.SetDefault("web_fetch.max_domain_concurrency", 5)
	v.SetDefault("web_fetch.cache_ttl", 30*time.Minute)
	v.SetDefault("web_fetch.max_url_length", 2000)
	v.SetDefault("web_fetch.max_content_size", 10*1024*1024)
	v.SetDefault("web_fetch.fetch_timeout", 60*time.Second)
	v.SetDefault("web_fetch.max_redirects", 10)
	v.SetDefault("web_fetch.llm_extract_threshold", 50000)
	v.SetDefault("web_fetch.llm_max_tokens", 4096)
	v.SetDefault("web_fetch.max_llm_per_session", 50)
	v.SetDefault("web_fetch.rate_limit.global_rps", 20.0)
	v.SetDefault("web_fetch.rate_limit.global_burst", 40)
	v.SetDefault("web_fetch.rate_limit.domain_rps", 2.0)
	v.SetDefault("web_fetch.rate_limit.domain_burst", 5)
	v.SetDefault("web_fetch.robots_cache_ttl", 24*time.Hour)
	v.SetDefault("web_fetch.user_agent", "RTCAgent-WebFetch/1.0")
	v.SetDefault("web_fetch.respect_robots_txt", true)
}

func setStorageDefaults(v *viper.Viper) {
	// Storage (rtc-oss3) defaults — disabled by default
	v.SetDefault("storage.backend", "")

	// MinIO defaults
	v.SetDefault("storage.minio.endpoint", "http://localhost:29000")
	v.SetDefault("storage.minio.access_key", "")
	v.SetDefault("storage.minio.secret_key", "")
	v.SetDefault("storage.minio.bucket", "rtc-agent")
	v.SetDefault("storage.minio.use_ssl", false)
	v.SetDefault("storage.minio.max_idle_conns", 1024)
	v.SetDefault("storage.minio.max_idle_conns_per_host", 100)
	v.SetDefault("storage.minio.idle_conn_timeout", 90*time.Second)
	v.SetDefault("storage.minio.max_retries", 3)
	v.SetDefault("storage.minio.retry_timeout", 30*time.Second)

	// S3 endpoint defaults
	v.SetDefault("storage.s3_endpoint.host", "0.0.0.0")
	v.SetDefault("storage.s3_endpoint.port", 9000)
	v.SetDefault("storage.s3_endpoint.tls_cert", "")
	v.SetDefault("storage.s3_endpoint.tls_key", "")
	v.SetDefault("storage.s3_endpoint.allowed_origins", []string{})
	v.SetDefault("storage.s3_endpoint.region", "us-east-1")

	// Quota defaults
	v.SetDefault("storage.quota.max_file_size_bytes", 100*1024*1024)   // 100MB
	v.SetDefault("storage.quota.max_user_quota_bytes", 1024*1024*1024) // 1GB
	v.SetDefault("storage.quota.max_concurrent_uploads", 10)
	v.SetDefault("storage.quota.pending_ttl", 5*time.Minute)

	// Rate limit defaults
	v.SetDefault("storage.rate_limit.requests_per_minute", 60)

	// Credential defaults
	v.SetDefault("storage.credential.access_token_ttl", 1*time.Hour)
	v.SetDefault("storage.credential.session_token_ttl", 1*time.Hour)
	v.SetDefault("storage.credential.presigned_url_ttl", 1*time.Hour)

	// Cleanup defaults
	v.SetDefault("storage.cleanup.interval", 1*time.Hour)
	v.SetDefault("storage.cleanup.multipart_expiry", 24*time.Hour)
	v.SetDefault("storage.cleanup.credential_expiry", 24*time.Hour)

	// Orphan cleanup defaults (conservative: disabled, dry-run)
	v.SetDefault("storage.cleanup.orphan.enabled", false)
	v.SetDefault("storage.cleanup.orphan.batch_size", 500)
	v.SetDefault("storage.cleanup.orphan.cooldown_period", 1*time.Hour)
	v.SetDefault("storage.cleanup.orphan.dry_run", true)

	// Encryption defaults
	v.SetDefault("storage.encryption.session_token_key", "")
}
