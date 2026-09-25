package websearch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/circuitbreaker"
	"github.com/leichujun/rtc-agent/server/pkg/proxy"
	"go.uber.org/zap"
)

// WebSearchConfig defines the configuration for WebSearchManager
type WebSearchConfig struct {
	BalancerType    string                       `json:"balancer_type"` // round_robin, weighted
	Providers       []ProviderConfig             `json:"providers"`
	Proxies         []proxy.ProxyConfig          `json:"proxies"`
	CircuitBreaker  circuitbreaker.CircuitBreakerConfig `json:"circuit_breaker"`
	RateLimiter     RateLimiterConfig            `json:"rate_limiter"`
	Retry           RetryConfig                  `json:"retry"`
	GlobalTimeout   time.Duration                `json:"global_timeout"`
	ProxyHealthURL  string                       `json:"proxy_health_url"`  // URL for proxy health checks
	ProxyCheckInterval time.Duration             `json:"proxy_check_interval"` // Proxy health check interval
}

// ProviderConfig defines provider configuration
type ProviderConfig struct {
	Name    string `json:"name"`    // Provider name
	Type    string `json:"type"`    // Provider type
	Weight  int    `json:"weight"`  // Load balancer weight (0-100)
	Enabled bool   `json:"enabled"` // Whether enabled
}

// RetryConfig defines retry parameters
type RetryConfig struct {
	MaxRetries    int           `json:"max_retries"`    // Maximum retry attempts
	RetryDelay    time.Duration `json:"retry_delay"`    // Initial retry delay
	BackoffFactor float64       `json:"backoff_factor"` // Exponential backoff factor
}

// DefaultWebSearchConfig returns sensible defaults
func DefaultWebSearchConfig() WebSearchConfig {
	return WebSearchConfig{
		BalancerType:   "round_robin",
		CircuitBreaker: circuitbreaker.DefaultCircuitBreakerConfig(),
		RateLimiter:    DefaultRateLimiterConfig(),
		Retry: RetryConfig{
			MaxRetries:    3,
			RetryDelay:    1 * time.Second,
			BackoffFactor: 2.0,
		},
		GlobalTimeout: 30 * time.Second,
	}
}

// WebSearchManager orchestrates search providers with load balancing,
// circuit breaking, rate limiting, and automatic failover.
type WebSearchManager struct {
	providers    []WebSearchProvider
	balancer     Balancer
	breakers     map[string]*circuitbreaker.CircuitBreaker
	rateLimiters map[string]*RateLimiter
	proxyPool    *proxy.ProxyPool
	config       WebSearchConfig
	logger       *zap.Logger

	shutdownCh   chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
}

// NewWebSearchManager creates a new WebSearchManager
func NewWebSearchManager(cfg WebSearchConfig, providers []WebSearchProvider, logger *zap.Logger) (*WebSearchManager, error) {
	if len(providers) == 0 {
		return nil, errors.New("at least one provider required")
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	// Create balancer
	var balancer Balancer
	switch cfg.BalancerType {
	case "weighted":
		weights := make(map[string]int)
		for _, p := range providers {
			// Try to find matching provider config for weight
			weight := 1 // default weight
			for _, pc := range cfg.Providers {
				if pc.Name == p.Name() && pc.Weight > 0 {
					weight = pc.Weight
					break
				}
			}
			weights[p.Name()] = weight
		}
		balancer = NewWeightedRandomBalancer(weights)
	default:
		balancer = NewRoundRobinBalancer()
	}

	// Create circuit breakers and rate limiters per provider
	breakers := make(map[string]*circuitbreaker.CircuitBreaker)
	rateLimiters := make(map[string]*RateLimiter)
	for _, p := range providers {
		breakers[p.Name()] = circuitbreaker.NewCircuitBreaker(cfg.CircuitBreaker, p.Name())
		rateLimiters[p.Name()] = NewRateLimiter(cfg.RateLimiter.Rate, cfg.RateLimiter.Burst)
	}

	// Create proxy pool if proxies configured
	var proxyPool *proxy.ProxyPool
	if len(cfg.Proxies) > 0 {
		proxyPool = proxy.NewProxyPool(cfg.Proxies, cfg.ProxyHealthURL, cfg.ProxyCheckInterval, logger)
	}

	return &WebSearchManager{
		providers:    providers,
		balancer:     balancer,
		breakers:     breakers,
		rateLimiters: rateLimiters,
		proxyPool:    proxyPool,
		config:       cfg,
		logger:       logger,
		shutdownCh:   make(chan struct{}),
	}, nil
}

// Search executes a search with load balancing and automatic failover
func (m *WebSearchManager) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	// Check shutdown state
	select {
	case <-m.shutdownCh:
		return nil, errors.New("manager is shutting down")
	default:
	}

	// Apply global timeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < m.config.GlobalTimeout {
			// Caller has shorter deadline, keep original ctx
		} else {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, m.config.GlobalTimeout)
			defer cancel()
		}
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.config.GlobalTimeout)
		defer cancel()
	}

	// Get available providers
	// Note: len(m.providers) == 0 check is redundant since NewWebSearchManager validates it

	// Main request + failover retry with backoff
	maxRetries := min(m.config.Retry.MaxRetries, len(m.providers)-1)
	var lastErr error
	triedProviderNames := make(map[string]struct{})

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Check context before each attempt
		if ctx.Err() != nil {
			return nil, fmt.Errorf("search aborted: %w", ctx.Err())
		}

		// Filter out tried providers
		filtered := m.filterProviders(m.providers, triedProviderNames)
		if len(filtered) == 0 {
			break // All providers tried
		}

		// Select provider via load balancer
		provider, err := m.balancer.Select(ctx, filtered)
		if err != nil {
			return nil, fmt.Errorf("no available provider: %w", err)
		}
		triedProviderNames[provider.Name()] = struct{}{}

		// Check circuit breaker state first (fast path)
		if m.breakers[provider.Name()].IsOpen() {
			lastErr = fmt.Errorf("provider %s circuit open", provider.Name())
			continue
		}

		// Check rate limiter
		if !m.rateLimiters[provider.Name()].Allow() {
			lastErr = fmt.Errorf("provider %s rate limited", provider.Name())
			continue
		}

		// Select proxy and inject into context
		providerCtx := ctx
		if m.proxyPool != nil {
			if px := m.proxyPool.NextHealthy(); px != nil {
				providerCtx = WithProxy(ctx, px)
			}
		}

		// Execute search with circuit breaker protection
		start := time.Now()
		result, err := m.breakers[provider.Name()].Execute(func() (any, error) {
			return provider.Search(providerCtx, req)
		})
		duration := time.Since(start)

		if err != nil {
			m.logger.Warn("search failed",
				zap.String("provider", provider.Name()),
				zap.Error(err),
				zap.Duration("duration", duration),
				zap.Int("attempt", attempt),
			)
			lastErr = err

			// Exponential backoff + jitter
			if attempt < maxRetries {
				backoff := m.calculateBackoff(attempt)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return nil, fmt.Errorf("search aborted during backoff: %w", ctx.Err())
				}
			}
			continue
		}

		// Success - set metadata
		resp := result.(*SearchResponse)
		resp.Provider = provider.Name()
		resp.Duration = duration
		m.logger.Info("search success",
			zap.String("provider", provider.Name()),
			zap.Duration("duration", duration),
			zap.Int("results", len(resp.Results)),
		)

		return resp, nil
	}

	return nil, fmt.Errorf("all providers failed: %w", lastErr)
}

// context key for proxy injection
type contextKey string

const proxyContextKey contextKey = "websearch_proxy"

// WithProxy injects proxy into context
func WithProxy(ctx context.Context, p *proxy.Proxy) context.Context {
	return context.WithValue(ctx, proxyContextKey, p)
}

// ProxyFromContext retrieves proxy from context
func ProxyFromContext(ctx context.Context) *proxy.Proxy {
	p, _ := ctx.Value(proxyContextKey).(*proxy.Proxy)
	return p
}

// filterProviders returns providers not in tried set
func (m *WebSearchManager) filterProviders(
	providers []WebSearchProvider,
	tried map[string]struct{},
) []WebSearchProvider {
	filtered := make([]WebSearchProvider, 0, len(providers))
	for _, p := range providers {
		if _, ok := tried[p.Name()]; ok {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered
}

// calculateBackoff computes exponential backoff with jitter
func (m *WebSearchManager) calculateBackoff(attempt int) time.Duration {
	base := m.config.Retry.RetryDelay
	factor := m.config.Retry.BackoffFactor

	backoff := time.Duration(float64(base) * math.Pow(factor, float64(attempt)))
	if backoff > 5*time.Second {
		backoff = 5 * time.Second
	}

	// Prevent panic when backoff is 0
	if backoff <= 0 {
		return 0
	}

	jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
	return backoff/2 + jitter
}

// Start implements lifecycle.Component
func (m *WebSearchManager) Start(ctx context.Context) error {
	if len(m.providers) == 0 {
		return errors.New("no available providers")
	}

	// Start proxy pool health checking
	if m.proxyPool != nil {
		m.proxyPool.Start()
	}

	m.logger.Info("web search manager started",
		zap.Int("providers", len(m.providers)),
		zap.Bool("proxy_enabled", m.proxyPool != nil),
	)
	return nil
}

// Stop implements lifecycle.Component
func (m *WebSearchManager) Stop(ctx context.Context) error {
	var firstErr error

	m.shutdownOnce.Do(func() {
		close(m.shutdownCh)

		// Stop proxy pool
		if m.proxyPool != nil {
			m.proxyPool.Stop()
		}

		// Close all providers and collect errors
		var errs []error
		for _, p := range m.providers {
			if err := p.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			firstErr = errors.Join(errs...)
		}

		// Wait for background goroutines
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
		}

		m.logger.Info("web search manager stopped")
	})

	return firstErr
}

// HealthCheck implements lifecycle.Component
func (m *WebSearchManager) HealthCheck(ctx context.Context) error {
	if len(m.providers) == 0 {
		return errors.New("no available providers")
	}

	// Check proxy pool health
	if m.proxyPool != nil && !m.proxyPool.HasHealthyProxy() {
		return errors.New("no healthy proxies available")
	}

	// Check at least one provider is healthy and circuit closed
	for _, p := range m.providers {
		cb := m.breakers[p.Name()]
		if cb != nil && !cb.IsOpen() {
			if err := p.HealthCheck(ctx); err == nil {
				return nil // At least one provider is healthy
			}
		}
	}

	return errors.New("all providers unhealthy or circuit open")
}
