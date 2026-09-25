// Example demonstrates basic usage of the websearch package
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/websearch"
	"go.uber.org/zap"
)

// ExampleProvider is a mock provider for demonstration
type ExampleProvider struct {
	name string
}

func (p *ExampleProvider) Name() string { return p.name }

func (p *ExampleProvider) Search(ctx context.Context, req *websearch.SearchRequest) (*websearch.SearchResponse, error) {
	// Simulate search delay
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
	// 1. Create providers
	providers := []websearch.WebSearchProvider{
		&ExampleProvider{name: "provider-1"},
		&ExampleProvider{name: "provider-2"},
	}

	// 2. Configure manager
	cfg := websearch.DefaultWebSearchConfig()
	cfg.BalancerType = "round_robin"
	cfg.GlobalTimeout = 30 * time.Second

	// 3. Create manager
	logger, _ := zap.NewDevelopment()
	manager, err := websearch.NewWebSearchManager(cfg, providers, logger)
	if err != nil {
		log.Fatal(err)
	}

	// 4. Start manager (implements lifecycle.Component)
	ctx := context.Background()
	if err := manager.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer manager.Stop(ctx)

	// 5. Create Eino-compatible tool
	description := "Search the web for current information and return results with source links."
	searchTool, err := websearch.NewWebSearchTool(manager, logger, description)
	if err != nil {
		log.Fatal(err)
	}

	// 6. Get tool info (for LLM)
	info, err := searchTool.Info(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Tool: %s\n", info.Name)
	fmt.Printf("Description: %s\n", info.Desc)

	// 7. Execute search via manager directly
	req := &websearch.SearchRequest{
		Query:      "Go programming language",
		MaxResults: 10,
		TimeRange:  websearch.TimeRangeWeek,
	}

	resp, err := manager.Search(ctx, req)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\nSearch Results (%d total):\n", len(resp.Results))
	for i, result := range resp.Results {
		fmt.Printf("%d. %s\n", i+1, result.Title)
		fmt.Printf("   URL: %s\n", result.URL)
		fmt.Printf("   Source: %s\n", result.Source)
	}

	// 8. Health check
	if err := manager.HealthCheck(ctx); err != nil {
		log.Printf("Health check failed: %v", err)
	} else {
		fmt.Println("\nHealth check: OK")
	}
}
