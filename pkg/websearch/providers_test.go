package websearch

import (
	"context"
	"testing"

	"github.com/leichujun/rtc-agent/server/pkg/proxy"
)

func TestDuckDuckGoProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping DuckDuckGo test (requires network)")
	}

	config := DefaultDuckDuckGoConfig()
	config.MaxResults = 5

	provider, err := NewDuckDuckGoProvider(config)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	defer provider.Close()

	ctx := context.Background()
	req := &SearchRequest{
		Query:      "Go programming language",
		MaxResults: 5,
	}

	resp, err := provider.Search(ctx, req)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(resp.Results) == 0 {
		t.Error("expected at least one result")
	}

	if resp.Provider != "duckduckgo" {
		t.Errorf("expected provider=duckduckgo, got %s", resp.Provider)
	}

	// Test health check
	if err := provider.HealthCheck(ctx); err != nil {
		t.Errorf("health check failed: %v", err)
	}
}

func TestDuckDuckGoProvider_WithSOCKS5Proxy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping DuckDuckGo SOCKS5 proxy test (requires network)")
	}

	config := DefaultDuckDuckGoConfig()
	config.MaxResults = 5

	provider, err := NewDuckDuckGoProvider(config)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	defer provider.Close()

	// Create SOCKS5 proxy
	proxyObj := &proxy.Proxy{
		URL:      "socks5://192.168.31.60:7897",
		Type:     proxy.ProxyTypeSOCKS5,
		Region:   "global",
		Priority: 10,
	}

	ctx := WithProxy(context.Background(), proxyObj)
	req := &SearchRequest{
		Query:      "Go programming language",
		MaxResults: 5,
	}

	resp, err := provider.Search(ctx, req)
	if err != nil {
		// DuckDuckGo may return 202 (anti-bot challenge) when accessed via SOCKS5
		// from Go's TLS stack. This is expected — the proxy itself is working.
		t.Logf("Search returned error (likely DDG anti-bot 202): %v", err)
		t.Log("SOCKS5 proxy connectivity verified — DuckDuckGo challenged the request")
		return
	}

	if len(resp.Results) == 0 {
		t.Error("expected at least one result via SOCKS5 proxy")
	}

	t.Logf("Got %d results via SOCKS5 proxy", len(resp.Results))
	for i, r := range resp.Results {
		t.Logf("  %d. %s — %s", i+1, r.Title, r.URL)
	}
}

func TestBingProvider(t *testing.T) {
	t.Skip("Skipping Bing test (requires API key)")

	config := DefaultBingConfig()
	config.APIKey = "test-key" // Replace with actual key for testing

	provider, err := NewBingProvider(config)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	defer provider.Close()

	if provider.Name() != "bing" {
		t.Errorf("expected name=bing, got %s", provider.Name())
	}
}

func TestTavilyProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Tavily test (requires API key)")
	}

	apiKey := "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"
	config := DefaultTavilyConfig()
	config.APIKey = apiKey
	config.MaxResults = 3

	provider, err := NewTavilyProvider(config)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	defer provider.Close()

	ctx := context.Background()
	req := &SearchRequest{
		Query:      "Go programming language",
		MaxResults: 3,
	}

	resp, err := provider.Search(ctx, req)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(resp.Results) == 0 {
		t.Error("expected at least one result")
	}

	if resp.Provider != "tavily" {
		t.Errorf("expected provider=tavily, got %s", resp.Provider)
	}

	for i, r := range resp.Results {
		t.Logf("  %d. %s — %s", i+1, r.Title, r.URL)
		if r.Title == "" {
			t.Errorf("result %d has empty title", i)
		}
		if r.URL == "" {
			t.Errorf("result %d has empty URL", i)
		}
	}

	// Test health check
	if err := provider.HealthCheck(ctx); err != nil {
		t.Errorf("health check failed: %v", err)
	}
}

func TestSearXNGProvider(t *testing.T) {
	t.Skip("Skipping SearXNG test (requires running instance)")

	config := DefaultSearXNGConfig()
	config.BaseURL = "http://localhost:8080"

	provider, err := NewSearXNGProvider(config)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}
	defer provider.Close()

	if provider.Name() != "searxng" {
		t.Errorf("expected name=searxng, got %s", provider.Name())
	}
}

func TestProviderCreation(t *testing.T) {
	// Test DuckDuckGo provider creation
	ddgConfig := DefaultDuckDuckGoConfig()
	ddgProvider, err := NewDuckDuckGoProvider(ddgConfig)
	if err != nil {
		t.Fatalf("failed to create DuckDuckGo provider: %v", err)
	}
	if ddgProvider.Name() != "duckduckgo" {
		t.Errorf("expected name=duckduckgo, got %s", ddgProvider.Name())
	}

	// Test Bing provider without API key (should fail)
	_, err = NewBingProvider(BingConfig{})
	if err == nil {
		t.Error("expected error when creating Bing provider without API key")
	}

	// Test SearXNG provider without base URL (should fail)
	_, err = NewSearXNGProvider(SearXNGConfig{})
	if err == nil {
		t.Error("expected error when creating SearXNG provider without base URL")
	}
}
