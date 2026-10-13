package config

import (
	"time"
)

// WebSearchConfig holds the web search subsystem configuration.
// Controls search providers, proxies, circuit breaking, rate limiting, and retry.
type WebSearchConfig struct {
	// Enabled controls whether web search is active.
	Enabled bool `mapstructure:"enabled"`

	// BalancerType selects the load balancing strategy: "round_robin" or "weighted".
	BalancerType string `mapstructure:"balancer_type"`

	// Providers is the list of search provider configurations.
	Providers []WebSearchProviderConfig `mapstructure:"providers"`

	// Proxies is the list of proxy configurations for outbound search requests.
	Proxies []WebSearchProxyConfig `mapstructure:"proxies"`

	// CircuitBreaker holds circuit breaker parameters.
	CircuitBreaker WebSearchCircuitBreakerConfig `mapstructure:"circuit_breaker"`

	// RateLimiter holds rate limiter parameters.
	RateLimiter WebSearchRateLimiterConfig `mapstructure:"rate_limiter"`

	// Retry holds retry and backoff parameters.
	Retry WebSearchRetryConfig `mapstructure:"retry"`

	// GlobalTimeout is the per-search request timeout.
	GlobalTimeout time.Duration `mapstructure:"global_timeout"`

	// ProxyHealthURL is the URL used for proxy health checks.
	ProxyHealthURL string `mapstructure:"proxy_health_url"`

	// ProxyCheckInterval is the proxy health check interval.
	ProxyCheckInterval time.Duration `mapstructure:"proxy_check_interval"`

	// Redis holds the dedicated Redis configuration for distributed circuit breakers
	// and rate limiters. When Addr is empty, distributed features are disabled.
	Redis WebSearchRedisConfig `mapstructure:"redis"`
}

// WebSearchProviderConfig holds a single search provider configuration.
type WebSearchProviderConfig struct {
	// Name is the provider identifier (e.g., "tavily", "bing", "searxng").
	Name string `mapstructure:"name"`

	// Type is the provider type: "tavily", "bing", "searxng", "duckduckgo".
	Type string `mapstructure:"type"`

	// Weight is the load balancer weight (0-100).
	Weight int `mapstructure:"weight"`

	// Enabled controls whether this provider is active.
	Enabled bool `mapstructure:"enabled"`

	// Region is the search region (e.g., "us-en", "cn-zh"). Used by DuckDuckGo.
	Region string `mapstructure:"region"`

	// APIKey is the provider API key. Used by Bing.
	APIKey string `mapstructure:"api_key"`

	// BaseURL is the provider base URL. Used by SearXNG.
	BaseURL string `mapstructure:"base_url"`

	// MaxResults is the maximum number of results per query.
	MaxResults int `mapstructure:"max_results"`

	// Timeout is the per-provider request timeout.
	Timeout time.Duration `mapstructure:"timeout"`

	// Language is the preferred language for results. Used by SearXNG.
	Language string `mapstructure:"language"`

	// Market is the market code. Used by Bing (e.g., "en-US").
	Market string `mapstructure:"market"`

	// Endpoint is the API endpoint URL. Used by Bing.
	Endpoint string `mapstructure:"endpoint"`

	// SearchDepth controls the depth of search: "basic" or "advanced". Used by Tavily.
	SearchDepth string `mapstructure:"search_depth"`

	// IncludeAnswer whether to include a generated answer summary. Used by Tavily.
	IncludeAnswer bool `mapstructure:"include_answer"`
}

// WebSearchProxyConfig holds a single proxy configuration.
type WebSearchProxyConfig struct {
	// URL is the proxy URL (e.g., "socks5://user:pass@host:port").
	URL string `mapstructure:"url"`

	// Type is the proxy type: "socks5", "http", "https", "direct".
	Type string `mapstructure:"type"`

	// Region is the proxy region (e.g., "us", "cn", "global").
	Region string `mapstructure:"region"`

	// Priority is the proxy priority (1-10, higher is better).
	Priority int `mapstructure:"priority"`
}

// WebSearchCircuitBreakerConfig holds circuit breaker parameters.
type WebSearchCircuitBreakerConfig struct {
	// FailureThreshold is the failure percentage (0-100) to trigger circuit open.
	FailureThreshold int `mapstructure:"failure_threshold"`

	// OpenTimeout is the wait time before transitioning from open to half-open.
	OpenTimeout time.Duration `mapstructure:"open_timeout"`

	// HalfOpenMaxRequests is the max probe requests allowed in half-open state.
	HalfOpenMaxRequests int `mapstructure:"half_open_max_requests"`

	// WindowSize is the sliding window size (number of requests).
	WindowSize int `mapstructure:"window_size"`

	// WindowDuration is the sliding window time dimension.
	WindowDuration time.Duration `mapstructure:"window_duration"`
}

// WebSearchRateLimiterConfig holds rate limiter parameters.
type WebSearchRateLimiterConfig struct {
	// Rate is the allowed requests per second.
	Rate float64 `mapstructure:"rate"`

	// Burst is the maximum burst capacity.
	Burst int `mapstructure:"burst"`
}

// WebSearchRetryConfig holds retry and backoff parameters.
type WebSearchRetryConfig struct {
	// MaxRetries is the maximum retry attempts on failure.
	MaxRetries int `mapstructure:"max_retries"`

	// RetryDelay is the initial retry delay.
	RetryDelay time.Duration `mapstructure:"retry_delay"`

	// BackoffFactor is the exponential backoff multiplier.
	BackoffFactor float64 `mapstructure:"backoff_factor"`
}

// WebSearchRedisConfig holds the dedicated Redis configuration for distributed
// circuit breakers and rate limiters.
type WebSearchRedisConfig struct {
	// Addr is the Redis address. Empty disables distributed features.
	Addr string `mapstructure:"addr"`

	// Password is the Redis password.
	Password string `mapstructure:"password"`

	// DB is the Redis database number.
	DB int `mapstructure:"db"`
}

// WebFetchConfig holds the web fetch subsystem configuration.
// Controls fetching, caching, security, and concurrency limits.
type WebFetchConfig struct {
	// Enabled controls whether web fetch is active.
	Enabled bool `mapstructure:"enabled"`

	// MaxConcurrency is the global max concurrent fetches.
	MaxConcurrency int `mapstructure:"max_concurrency"`

	// MaxDomainConcurrency is the per-domain max concurrent fetches.
	MaxDomainConcurrency int `mapstructure:"max_domain_concurrency"`

	// CacheTTL is the cache time-to-live.
	CacheTTL time.Duration `mapstructure:"cache_ttl"`

	// MaxURLLength is the max URL length.
	MaxURLLength int `mapstructure:"max_url_length"`

	// MaxContentSize is the max content bytes.
	MaxContentSize int64 `mapstructure:"max_content_size"`

	// FetchTimeout is the request timeout.
	FetchTimeout time.Duration `mapstructure:"fetch_timeout"`

	// MaxRedirects is the max redirects to follow.
	MaxRedirects int `mapstructure:"max_redirects"`

	// LLMExtractThresholdBytes is the content size threshold to trigger LLM extraction.
	LLMExtractThresholdBytes int `mapstructure:"llm_extract_threshold"`

	// LLMMaxTokens is the max output tokens for LLM extraction.
	LLMMaxTokens int `mapstructure:"llm_max_tokens"`

	// MaxLLMExtractPerSession is the daily LLM extraction limit per session.
	MaxLLMExtractPerSession int `mapstructure:"max_llm_per_session"`

	// RateLimit holds dual-layer rate limiting configuration.
	RateLimit WebFetchRateLimitConfig `mapstructure:"rate_limit"`

	// RobotsCacheTTL is the robots.txt cache TTL.
	RobotsCacheTTL time.Duration `mapstructure:"robots_cache_ttl"`

	// UserAgent is the User-Agent header.
	UserAgent string `mapstructure:"user_agent"`

	// RespectRobotsTxt controls whether to respect robots.txt.
	RespectRobotsTxt bool `mapstructure:"respect_robots_txt"`

	// PreApprovedDomains is the list of pre-approved domains.
	PreApprovedDomains []string `mapstructure:"pre_approved_domains"`

	// BlockedDomains is the list of blocked domains.
	BlockedDomains []string `mapstructure:"blocked_domains"`

	// Redis holds the dedicated Redis configuration for caching and rate limiting.
	// When Addr is empty, the shared redis client is used.
	Redis WebFetchRedisConfig `mapstructure:"redis"`
}

// WebFetchRateLimitConfig holds dual-layer rate limiting configuration for web fetch.
type WebFetchRateLimitConfig struct {
	// GlobalRPS is the global requests per second.
	GlobalRPS float64 `mapstructure:"global_rps"`
	// GlobalBurst is the global burst size.
	GlobalBurst int `mapstructure:"global_burst"`
	// DomainRPS is the per-domain requests per second.
	DomainRPS float64 `mapstructure:"domain_rps"`
	// DomainBurst is the per-domain burst size.
	DomainBurst int `mapstructure:"domain_burst"`
}

// WebFetchRedisConfig holds the dedicated Redis configuration for web fetch
// caching and rate limiting.
type WebFetchRedisConfig struct {
	// Addr is the Redis address. Empty falls back to the shared redis client.
	Addr string `mapstructure:"addr"`

	// Password is the Redis password.
	Password string `mapstructure:"password"`

	// DB is the Redis database number.
	DB int `mapstructure:"db"`
}
