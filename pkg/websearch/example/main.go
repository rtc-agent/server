// Example demonstrates basic usage of the websearch package.
//
// The Eino InvokableTool adapter has moved to internal/agent/tools_web_search.go.
// This example shows how to use the WebSearchManager directly.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/websearch"
	"go.uber.org/zap"
)

// ExampleProvider is a mock provider for demonstration.
type ExampleProvider struct {
	name string
}

func (p *ExampleProvider) Name() string { return p.name }

func (p *ExampleProvider) Search(ctx context.Context, req *websearch.SearchRequest) (*websearch.SearchResponse, error) {
	// Simulate search delay.
	time.Sleep(100 * time.Millisecond)

	return &websearch.SearchResponse{
		Results: []websearch.SearchResult{
			{
				Title:       "Example Result for: " + req.Query,
				URL:         "https://example.com/search?q=" + req.Query,
				Description: "This is an example search result",
				Source:      p.name,
			},
		},
		TotalCount: 1,
		Provider:   p.name,
		Duration:   100 * time.Millisecond,
	}, nil
}

func (p *ExampleProvider) HealthCheck(ctx context.Context) error {
	return nil
}

func (p *ExampleProvider) Close() error {
	return nil
}

func main() {
	// 1. Create providers.
	providers := []websearch.WebSearchProvider{
		&ExampleProvider{name: "provider-1"},
		&ExampleProvider{name: "provider-2"},
	}

	// 2. Configure manager.
	cfg := websearch.DefaultWebSearchConfig()
	cfg.BalancerType = "round_robin"
	cfg.GlobalTimeout = 30 * time.Second

	// 3. Create manager.
	logger, _ := zap.NewDevelopment()
	manager, err := websearch.NewWebSearchManager(cfg, providers, logger)
	if err != nil {
		log.Fatal(err)
	}

	// 4. Start manager (implements lifecycle.Component).
	ctx := context.Background()
	if err := manager.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer manager.Stop(ctx)

	// 5. Execute search via the manager.
	req := &websearch.SearchRequest{
		Query:      "Go programming language",
		MaxResults: 10,
		TimeRange:  websearch.TimeRangeWeek,
	}

	resp, err := manager.Search(ctx, req)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Search Results (%d total):\n", len(resp.Results))
	for i, result := range resp.Results {
		fmt.Printf("%d. %s\n", i+1, result.Title)
		fmt.Printf("   URL: %s\n", result.URL)
		fmt.Printf("   Source: %s\n", result.Source)
	}

	// 6. Health check.
	if err := manager.HealthCheck(ctx); err != nil {
		log.Printf("Health check failed: %v", err)
	} else {
		fmt.Println("\nHealth check: OK")
	}
}
