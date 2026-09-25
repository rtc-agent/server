package websearch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/proxy"
)

// ProxyType defines the type of proxy
type ProxyType string

const (
	ProxyTypeSOCKS5 ProxyType = "socks5"
	ProxyTypeHTTP   ProxyType = "http"
	ProxyTypeHTTPS  ProxyType = "https"
	ProxyTypeDirect ProxyType = "direct"
)

// ProxyConfig defines proxy configuration
type ProxyConfig struct {
	URL      string    `json:"url"`       // e.g., socks5://user:pass@host:port
	Type     ProxyType `json:"type"`      // socks5, http, https, direct
	Region   string    `json:"region"`    // e.g., us, cn, jp
	Priority int       `json:"priority"`  // Priority (1-10, higher is better)
}

// Proxy represents a proxy server
type Proxy struct {
	URL      string
	Type     ProxyType
	Region   string
	Priority int
	Auth     *proxy.Auth // Parsed from URL for SOCKS5
}

// ProxyHealth tracks proxy health metrics
type ProxyHealth struct {
	SuccessRate atomic.Uint64 // 0-10000 (0-100.00%, two decimal precision)
	AvgLatency  atomic.Int64  // nanoseconds
	LastCheck   atomic.Int64  // unix timestamp
}

// ProxyPool manages multiple proxies with health checking and rotation
type ProxyPool struct {
	proxies        []Proxy
	current        atomic.Uint64
	healthyCurrent atomic.Uint64 // Separate counter for NextHealthy
	healthMap      sync.Map // map[string]*ProxyHealth
	checkStop      chan struct{}
	stopOnce       sync.Once
	startOnce      sync.Once // guards Start() from being called multiple times
	wg             sync.WaitGroup
	healthCheckURL string
	checkInterval  time.Duration
	internalCtx    context.Context
	internalCancel context.CancelFunc
	logger         *zap.Logger
}

// NewProxyPool creates a new proxy pool
func NewProxyPool(configs []ProxyConfig, healthCheckURL string, checkInterval time.Duration, logger *zap.Logger) *ProxyPool {
	if logger == nil {
		logger = zap.NewNop()
	}

	proxies := make([]Proxy, 0, len(configs))
	for _, cfg := range configs {
		p := Proxy{
			URL:      cfg.URL,
			Type:     cfg.Type,
			Region:   cfg.Region,
			Priority: cfg.Priority,
		}

		// Parse authentication from URL for SOCKS5
		if cfg.Type == ProxyTypeSOCKS5 && cfg.URL != "" {
			if u, err := url.Parse(cfg.URL); err == nil && u.User != nil {
				password, _ := u.User.Password()
				p.Auth = &proxy.Auth{
					User:     u.User.Username(),
					Password: password,
				}
			}
		}

		proxies = append(proxies, p)
	}

	if healthCheckURL == "" {
		healthCheckURL = "https://www.google.com"
	}
	if checkInterval <= 0 {
		checkInterval = 30 * time.Second
	}

	return &ProxyPool{
		proxies:        proxies,
		checkStop:      make(chan struct{}),
		healthCheckURL: healthCheckURL,
		checkInterval:  checkInterval,
		logger:         logger,
	}
}

// Start begins background health checking.
// Safe to call multiple times; subsequent calls are no-ops.
func (p *ProxyPool) Start() {
	if len(p.proxies) == 0 {
		return
	}

	// Use sync.Once to prevent multiple calls from leaking goroutines
	p.startOnce.Do(func() {
		// Use internal context to decouple from caller's lifecycle
		p.internalCtx, p.internalCancel = context.WithCancel(context.Background())

		p.wg.Add(1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					p.logger.Error("proxy health check goroutine panic recovered",
						zap.Any("panic", r),
						zap.String("stack", string(debug.Stack())))
				}
			}()
			defer p.wg.Done()
			ticker := time.NewTicker(p.checkInterval)
			defer ticker.Stop()

			// Initial check
			p.checkAllProxies(p.internalCtx)

			for {
				select {
				case <-ticker.C:
					p.checkAllProxies(p.internalCtx)
				case <-p.checkStop:
					return
				case <-p.internalCtx.Done():
					return
				}
			}
		}()
	})
}

// Stop stops background health checking
func (p *ProxyPool) Stop() {
	p.stopOnce.Do(func() {
		close(p.checkStop)
		if p.internalCancel != nil {
			p.internalCancel()
		}
		p.wg.Wait()
	})
}

// Next returns the next proxy in round-robin order
// Returns nil if no proxies configured (direct connection)
func (p *ProxyPool) Next() *Proxy {
	if len(p.proxies) == 0 {
		return nil
	}
	idx := p.current.Add(1) % uint64(len(p.proxies))
	return &p.proxies[idx]
}

// NextHealthy returns the next healthy proxy
// Returns nil if no healthy proxies available
func (p *ProxyPool) NextHealthy() *Proxy {
	if len(p.proxies) == 0 {
		return nil
	}

	// Collect healthy candidates (success rate >= 90%)
	candidates := make([]*Proxy, 0, len(p.proxies))
	for i := range p.proxies {
		proxy := &p.proxies[i]
		if health, ok := p.healthMap.Load(proxy.URL); ok {
			h := health.(*ProxyHealth)
			if h.SuccessRate.Load() >= 9000 {
				candidates = append(candidates, proxy)
			}
		} else {
			// No health data yet, include as candidate
			candidates = append(candidates, proxy)
		}
	}

	if len(candidates) == 0 {
		// Fallback: return any proxy
		return p.Next()
	}

	// Use separate counter for healthy candidates to ensure even distribution
	idx := p.healthyCurrent.Add(1) % uint64(len(candidates))
	return candidates[idx]
}

// HasHealthyProxy checks if at least one healthy proxy exists
func (p *ProxyPool) HasHealthyProxy() bool {
	if len(p.proxies) == 0 {
		return true // No proxy config means direct connection is OK
	}

	hasAnyHealthData := false
	for i := range p.proxies {
		proxy := &p.proxies[i]
		if health, ok := p.healthMap.Load(proxy.URL); ok {
			hasAnyHealthData = true
			h := health.(*ProxyHealth)
			if h.SuccessRate.Load() >= 9000 {
				return true
			}
		}
	}

	// If no health data yet, assume healthy
	if !hasAnyHealthData {
		return true
	}

	return false
}

// GetHealth returns health metrics for a proxy
func (p *ProxyPool) GetHealth(proxyURL string) *ProxyHealth {
	if health, ok := p.healthMap.Load(proxyURL); ok {
		return health.(*ProxyHealth)
	}
	return nil
}

// checkAllProxies checks health of all proxies in parallel
func (p *ProxyPool) checkAllProxies(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(len(p.proxies))

	for i := range p.proxies {
		proxy := &p.proxies[i]
		health, _ := p.healthMap.LoadOrStore(proxy.URL, &ProxyHealth{})
		h := health.(*ProxyHealth)

		go func(proxy *Proxy, h *ProxyHealth) {
			defer func() {
				if r := recover(); r != nil {
					p.logger.Error("proxy health check panic recovered",
						zap.String("proxy_url", proxy.URL),
						zap.Any("panic", r),
						zap.String("stack", string(debug.Stack())))
				}
			}()
			defer wg.Done()

			// Check proxy health
			start := time.Now()
			err := p.pingProxy(ctx, proxy, 5*time.Second)
			latency := time.Since(start)

			// Update metrics using EMA (Exponential Moving Average)
			updateEMA(&h.SuccessRate, err == nil)
			updateEMAInt64(&h.AvgLatency, int64(latency))
			h.LastCheck.Store(time.Now().Unix())
		}(proxy, h)
	}

	wg.Wait()
}

// pingProxy tests if a proxy is reachable
func (p *ProxyPool) pingProxy(ctx context.Context, proxy *Proxy, timeout time.Duration) error {
	client, err := createHTTPClient(proxy)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.healthCheckURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}

	return nil
}

// updateEMA updates success rate using Exponential Moving Average
func updateEMA(val *atomic.Uint64, success bool) {
	const alpha = 0.3 // Smoothing factor
	for {
		old := val.Load()
		var newVal uint64
		if success {
			newVal = uint64(float64(old)*(1-alpha) + 10000*alpha) // 10000 = 100.00%
		} else {
			newVal = uint64(float64(old) * (1 - alpha))
		}
		if val.CompareAndSwap(old, newVal) {
			break
		}
	}
}

// updateEMAInt64 updates latency using Exponential Moving Average
func updateEMAInt64(val *atomic.Int64, newVal int64) {
	const alpha = 0.3
	for {
		old := val.Load()
		updated := int64(float64(old)*(1-alpha) + float64(newVal)*alpha)
		if val.CompareAndSwap(old, updated) {
			break
		}
	}
}

// createHTTPClient creates an HTTP client configured to use the proxy
func createHTTPClient(p *Proxy) (*http.Client, error) {
	if p == nil || p.Type == ProxyTypeDirect {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}

	transport := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	switch p.Type {
	case ProxyTypeHTTP, ProxyTypeHTTPS:
		proxyURL, err := url.Parse(p.URL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)

	case ProxyTypeSOCKS5:
		u, err := url.Parse(p.URL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}

		// Create SOCKS5 dialer
		dialer, err := proxy.SOCKS5("tcp", u.Host, p.Auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}

		// Type assert to ContextDialer with protection
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS5 dialer does not implement proxy.ContextDialer")
		}

		// Inject into Transport
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return ctxDialer.DialContext(ctx, network, addr)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}, nil
}
