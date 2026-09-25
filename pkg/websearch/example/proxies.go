package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/proxy"
	"github.com/leichujun/rtc-agent/server/pkg/websearch"
	"go.uber.org/zap"
)

// ExampleProxies demonstrates using proxy pool with web search
func ExampleProxies() {
	// 1. Create provider
	ddgConfig := websearch.DefaultDuckDuckGoConfig()
	ddgProvider, err := websearch.NewDuckDuckGoProvider(ddgConfig)
	if err != nil {
		log.Fatal(err)
	}
	defer ddgProvider.Close()

	providers := []websearch.WebSearchProvider{ddgProvider}

	// 2. Configure proxies
	cfg := websearch.DefaultWebSearchConfig()
	cfg.BalancerType = "round_robin"
	cfg.Proxies = []proxy.ProxyConfig{
		{
			URL:      "socks5://user:pass@proxy1.example.com:1080",
			Type:     proxy.ProxyTypeSOCKS5,
			Region:   "us",
			Priority: 10,
		},
		{
			URL:      "http://proxy2.example.com:8080",
			Type:     proxy.ProxyTypeHTTP,
			Region:   "jp",
			Priority: 5,
		},
	}
	cfg.ProxyHealthURL = "https://www.google.com"
	cfg.ProxyCheckInterval = 30 * time.Second

	// 3. Create manager with proxy support
	logger, _ := zap.NewDevelopment()
	manager, err := websearch.NewWebSearchManager(cfg, providers, logger)
	if err != nil {
		log.Fatal(err)
	}

	// 4. Start manager (starts proxy health checking)
	ctx := context.Background()
	if err := manager.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer manager.Stop(ctx)

	// 5. Execute search (will use healthy proxies automatically)
	req := &websearch.SearchRequest{
		Query:      "Go programming language",
		MaxResults: 10,
	}

	resp, err := manager.Search(ctx, req)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Found %d results via proxy\n", len(resp.Results))
	for i, result := range resp.Results {
		fmt.Printf("%d. %s\n", i+1, result.Title)
	}

	// 6. Check health (includes proxy health)
	if err := manager.HealthCheck(ctx); err != nil {
		log.Printf("Health check failed: %v", err)
	} else {
		fmt.Println("All systems healthy")
	}
}

// ExampleProxyTypes demonstrates different proxy types
func ExampleProxyTypes() {
	// SOCKS5 proxy with authentication
	socks5Proxy := proxy.ProxyConfig{
		URL:      "socks5://username:password@proxy.example.com:1080",
		Type:     proxy.ProxyTypeSOCKS5,
		Region:   "us",
		Priority: 10,
	}

	// HTTP proxy
	httpProxy := proxy.ProxyConfig{
		URL:      "http://proxy.example.com:8080",
		Type:     proxy.ProxyTypeHTTP,
		Region:   "jp",
		Priority: 5,
	}

	// HTTPS proxy
	httpsProxy := proxy.ProxyConfig{
		URL:      "https://proxy.example.com:8443",
		Type:     proxy.ProxyTypeHTTPS,
		Region:   "sg",
		Priority: 8,
	}

	proxies := []proxy.ProxyConfig{socks5Proxy, httpProxy, httpsProxy}

	// Create proxy pool
	pool := proxy.NewProxyPool(proxies, "https://www.google.com", 30*time.Second, nil)

	pool.Start()
	defer pool.Stop()

	// Get next healthy proxy
	proxy := pool.NextHealthy()
	if proxy != nil {
		fmt.Printf("Using proxy: %s (region: %s)\n", proxy.URL, proxy.Region)
	}
}

// ExampleProxyHealth demonstrates proxy health monitoring
func ExampleProxyHealth() {
	proxies := []proxy.ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: proxy.ProxyTypeHTTP},
		{URL: "http://proxy2.example.com:8080", Type: proxy.ProxyTypeHTTP},
	}

	pool := proxy.NewProxyPool(proxies, "https://www.google.com", 10*time.Second, nil)

	pool.Start()
	defer pool.Stop()

	// Wait for health checks
	time.Sleep(15 * time.Second)

	// Check health of each proxy
	for _, p := range proxies {
		health := pool.GetHealth(p.URL)
		if health != nil {
			successRate := float64(health.SuccessRate.Load()) / 100.0
			avgLatency := time.Duration(health.AvgLatency.Load())
			lastCheck := time.Unix(health.LastCheck.Load(), 0)

			fmt.Printf("Proxy: %s\n", p.URL)
			fmt.Printf("  Success Rate: %.2f%%\n", successRate)
			fmt.Printf("  Avg Latency: %v\n", avgLatency)
			fmt.Printf("  Last Check: %v\n", lastCheck)
		}
	}
}
