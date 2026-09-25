package websearch

import (
	"context"
	"testing"
)

func TestDuckDuckGoProvider(t *testing.T) {
	t.Skip("Skipping DuckDuckGo test (requires network)")

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
