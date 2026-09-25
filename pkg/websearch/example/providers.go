package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/websearch"
	"go.uber.org/zap"
)

// ExampleProviders demonstrates creating and using multiple providers
func ExampleProviders() {
	// 1. Create DuckDuckGo provider (free, no API key needed)
	ddgConfig := websearch.DefaultDuckDuckGoConfig()
	ddgConfig.MaxResults = 10
	ddgProvider, err := websearch.NewDuckDuckGoProvider(ddgConfig)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Create Bing provider (requires API key)
	// bingConfig := websearch.DefaultBingConfig()
	// bingConfig.APIKey = os.Getenv("BING_API_KEY")
	// bingProvider, err := websearch.NewBingProvider(bingConfig)
	// if err != nil {
	//     log.Fatal(err)
	// }

	// 3. Create SearXNG provider (requires running instance)
	// searxngConfig := websearch.DefaultSearXNGConfig()
	// searxngConfig.BaseURL = "http://localhost:8080"
	// searxngProvider, err := websearch.NewSearXNGProvider(searxngConfig)
	// if err != nil {
	//     log.Fatal(err)
	// }

	// 4. Combine providers
	providers := []websearch.WebSearchProvider{
		ddgProvider,
		// bingProvider,
		// searxngProvider,
	}

	// 5. Create manager with multiple providers
	cfg := websearch.DefaultWebSearchConfig()
	cfg.BalancerType = "weighted"
	cfg.Providers = []websearch.ProviderConfig{
		{Name: "duckduckgo", Weight: 40},
		// {Name: "bing", Weight: 50},
		// {Name: "searxng", Weight: 10},
	}

	logger, _ := zap.NewDevelopment()
	manager, err := websearch.NewWebSearchManager(cfg, providers, logger)
	if err != nil {
		log.Fatal(err)
	}

	// 6. Start manager
	ctx := context.Background()
	if err := manager.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer manager.Stop(ctx)

	// 7. Execute search
	req := &websearch.SearchRequest{
		Query:      "Go programming language tutorial",
		MaxResults: 10,
		TimeRange:  websearch.TimeRangeYear,
	}

	resp, err := manager.Search(ctx, req)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Found %d results in %v\n", len(resp.Results), resp.Duration)
	for i, result := range resp.Results {
		fmt.Printf("%d. %s (%s)\n", i+1, result.Title, result.Source)
	}

	// 8. Health check
	if err := manager.HealthCheck(ctx); err != nil {
		log.Printf("Health check failed: %v", err)
	}
}

// ExampleWithTimeout demonstrates timeout handling
func ExampleWithTimeout() {
	provider, _ := websearch.NewDuckDuckGoProvider(websearch.DefaultDuckDuckGoConfig())
	defer provider.Close()

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &websearch.SearchRequest{
		Query:      "test query",
		MaxResults: 5,
	}

	resp, err := provider.Search(ctx, req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Println("Search timed out")
		} else {
			log.Printf("Search failed: %v", err)
		}
		return
	}

	fmt.Printf("Found %d results\n", len(resp.Results))
}

// ExampleFailover demonstrates automatic failover
func ExampleFailover() {
	// Create multiple providers for failover
	provider1, _ := websearch.NewDuckDuckGoProvider(websearch.DefaultDuckDuckGoConfig())
	provider2, _ := websearch.NewDuckDuckGoProvider(websearch.DefaultDuckDuckGoConfig())

	providers := []websearch.WebSearchProvider{
		provider1,
		provider2,
	}
	defer provider1.Close()
	defer provider2.Close()

	cfg := websearch.DefaultWebSearchConfig()
	cfg.Retry.MaxRetries = 2
	cfg.BalancerType = "round_robin"

	manager, _ := websearch.NewWebSearchManager(cfg, providers, nil)
	defer manager.Stop(context.Background())

	ctx := context.Background()
	req := &websearch.SearchRequest{Query: "test query"}

	// Manager will try providers with automatic failover on failure
	resp, err := manager.Search(ctx, req)
	if err != nil {
		log.Printf("Search failed: %v", err)
		return
	}

	fmt.Printf("Success with provider: %s, found %d results\n", resp.Provider, len(resp.Results))
}
