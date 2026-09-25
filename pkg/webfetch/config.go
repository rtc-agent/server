package webfetch

import (
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/proxy"
)

// WebFetchConfig holds all configuration for the WebFetchManager.
// Uses mapstructure tags for Viper deserialization.
type WebFetchConfig struct {
	// Enabled controls whether web fetch is active.
	Enabled bool `mapstructure:"enabled"`

	// Concurrency control.
	MaxConcurrency       int `mapstructure:"max_concurrency"`        // global max concurrent fetches, default 20
	MaxDomainConcurrency int `mapstructure:"max_domain_concurrency"` // per-domain max concurrent fetches, default 5

	// Cache.
	CacheTTL     time.Duration `mapstructure:"cache_ttl"`      // cache TTL, default 30min
	CacheMaxSize int           `mapstructure:"cache_max_size"` // max cache entries, default 1000

	// Request control.
	MaxURLLength   int           `mapstructure:"max_url_length"`   // max URL length, default 2000
	MaxContentSize int64         `mapstructure:"max_content_size"` // max content bytes, default 10MB
	FetchTimeout   time.Duration `mapstructure:"fetch_timeout"`    // request timeout, default 60s
	MaxRedirects   int           `mapstructure:"max_redirects"`    // max redirects, default 10

	// LLM extraction.
	LLMExtractThresholdBytes int           `mapstructure:"llm_extract_threshold"` // bytes threshold to trigger LLM extraction, default 50000
	LLMCacheTTL             time.Duration `mapstructure:"llm_cache_ttl"`          // LLM result cache TTL, default 1h
	MaxLLMExtractPerSession int           `mapstructure:"max_llm_per_session"`    // daily LLM extraction limit per session, default 50
	LLMMaxTokens            int           `mapstructure:"llm_max_tokens"`         // max output tokens for LLM extraction, default 4096

	// Rate limiting.
	RateLimit RateLimitConfig `mapstructure:"rate_limit"` // dual-layer rate limiting config

	// Robots.txt.
	RespectRobotsTxt bool          `mapstructure:"respect_robots_txt"` // whether to respect robots.txt, default true
	RobotsCacheTTL   time.Duration `mapstructure:"robots_cache_ttl"`   // robots.txt cache TTL, default 24h

	// Circuit breaker (Phase 3).
	CircuitBreaker CircuitBreakerConfig `mapstructure:"circuit_breaker"` // per-domain circuit breaker config

	// Proxy pool (Phase 3).
	Proxies            []proxy.ProxyConfig `mapstructure:"proxies"`              // proxy list
	ProxyHealthURL     string              `mapstructure:"proxy_health_url"`     // URL for proxy health checks
	ProxyCheckInterval time.Duration       `mapstructure:"proxy_check_interval"` // proxy health check interval

	// Distributed cache sync (Phase 3).
	CacheSyncEnabled  bool   `mapstructure:"cache_sync_enabled"`  // whether to enable distributed cache sync
	CacheSyncChannel  string `mapstructure:"cache_sync_channel"`  // Redis Pub/Sub channel for cache sync
	CacheSyncSourceID string `mapstructure:"cache_sync_source_id"` // unique ID for this instance

	// Security.
	PreApprovedDomains []string `mapstructure:"pre_approved_domains"` // pre-approved domain list
	BlockedDomains     []string `mapstructure:"blocked_domains"`      // blocked domain list
	AllowedSchemes     []string `mapstructure:"allowed_schemes"`      // allowed URL schemes, default ["https", "http"]

	// Other.
	UserAgent string `mapstructure:"user_agent"` // User-Agent header, default "RTCAgent-WebFetch/1.0"
}

// CircuitBreakerConfig defines circuit breaker configuration for webfetch
type CircuitBreakerConfig struct {
	Enabled             bool          `mapstructure:"enabled"`              // whether to enable circuit breaker
	FailureThreshold    int           `mapstructure:"failure_threshold"`    // 0-100, percentage of failures to trigger open
	OpenTimeout         time.Duration `mapstructure:"open_timeout"`         // wait time before half-open
	HalfOpenMaxRequests int           `mapstructure:"half_open_max_requests"` // probe requests in half-open
	WindowSize          int           `mapstructure:"window_size"`          // sliding window size
	WindowDuration      time.Duration `mapstructure:"window_duration"`      // window time dimension
}

// DefaultCircuitBreakerConfig returns sensible defaults for webfetch
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		Enabled:             false,
		FailureThreshold:    50,
		OpenTimeout:         30 * time.Second,
		HalfOpenMaxRequests: 3,
		WindowSize:          20,
		WindowDuration:      1 * time.Minute,
	}
}

// DefaultWebFetchConfig returns a WebFetchConfig with sensible defaults.
func DefaultWebFetchConfig() WebFetchConfig {
	return WebFetchConfig{
		Enabled:                    false,
		MaxConcurrency:             20,
		MaxDomainConcurrency:       5,
		CacheTTL:                   30 * time.Minute,
		CacheMaxSize:               1000,
		MaxURLLength:               2000,
		MaxContentSize:             10 * 1024 * 1024, // 10MB
		FetchTimeout:               60 * time.Second,
		MaxRedirects:               10,
		LLMExtractThresholdBytes:   50000,
		LLMCacheTTL:                1 * time.Hour,
		MaxLLMExtractPerSession:    50,
		LLMMaxTokens:               4096,
		RateLimit:                  DefaultRateLimitConfig(),
		RespectRobotsTxt:           true,
		RobotsCacheTTL:             24 * time.Hour,
		CircuitBreaker:             DefaultCircuitBreakerConfig(),
		AllowedSchemes:             []string{"https", "http"},
		UserAgent:                  "RTCAgent-WebFetch/1.0",
		PreApprovedDomains:         defaultPreApprovedDomains(),
	}
}

// defaultPreApprovedDomains returns the built-in list of pre-approved domains.
func defaultPreApprovedDomains() []string {
	return []string{
		// Anthropic
		"platform.claude.com", "code.claude.com", "modelcontextprotocol.io",
		"github.com/anthropics", "agentskills.io",
		// Programming language docs
		"docs.python.org", "en.cppreference.com", "docs.oracle.com",
		"learn.microsoft.com", "developer.mozilla.org", "go.dev", "pkg.go.dev",
		"www.php.net", "docs.swift.org", "kotlinlang.org", "ruby-doc.org",
		"doc.rust-lang.org", "www.typescriptlang.org",
		// Web & JavaScript
		"react.dev", "angular.io", "vuejs.org", "nextjs.org", "expressjs.com",
		"nodejs.org", "bun.sh", "jquery.com", "getbootstrap.com", "tailwindcss.com",
		"d3js.org", "threejs.org", "redux.js.org", "webpack.js.org", "jestjs.io",
		"reactrouter.com",
		// Python
		"docs.djangoproject.com", "flask.palletsprojects.com", "fastapi.tiangolo.com",
		"pandas.pydata.org", "numpy.org", "www.tensorflow.org", "pytorch.org",
		"scikit-learn.org", "matplotlib.org", "requests.readthedocs.io", "jupyter.org",
		// PHP
		"laravel.com", "symfony.com", "wordpress.org",
		// Java
		"docs.spring.io", "hibernate.org", "tomcat.apache.org", "gradle.org", "maven.apache.org",
		// .NET
		"asp.net", "dotnet.microsoft.com", "nuget.org", "blazor.net",
		// Mobile
		"reactnative.dev", "docs.flutter.dev", "developer.apple.com", "developer.android.com",
		// Data science & ML
		"keras.io", "spark.apache.org", "huggingface.co", "www.kaggle.com",
		// Databases
		"www.mongodb.com", "redis.io", "www.postgresql.org", "dev.mysql.com",
		"www.sqlite.org", "graphql.org", "prisma.io",
		// Cloud & DevOps
		"docs.aws.amazon.com", "cloud.google.com", "kubernetes.io", "www.docker.com",
		"www.terraform.io", "www.ansible.com", "vercel.com/docs", "docs.netlify.com",
		"devcenter.heroku.com",
		// Testing
		"cypress.io", "selenium.dev",
		// Game dev
		"docs.unity.com", "docs.unrealengine.com",
		// Developer communities
		"github.com", "stackoverflow.com",
		// Tools
		"git-scm.com", "nginx.org", "httpd.apache.org",
	}
}
