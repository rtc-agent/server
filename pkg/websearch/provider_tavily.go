package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TavilyConfig defines Tavily provider configuration
type TavilyConfig struct {
	APIKey     string        `json:"api_key"`     // Required: Tavily API key
	MaxResults int           `json:"max_results"` // Default: 10
	Timeout    time.Duration `json:"timeout"`     // Default: 30s
	// SearchDepth controls the depth of search: "basic" or "advanced"
	// "basic" is faster, "advanced" does more thorough research
	SearchDepth string `json:"search_depth"` // Default: "basic"
	// IncludeAnswer whether to include a generated answer summary
	IncludeAnswer bool `json:"include_answer"` // Default: false
}

// DefaultTavilyConfig returns sensible defaults
func DefaultTavilyConfig() TavilyConfig {
	return TavilyConfig{
		MaxResults:    10,
		Timeout:       30 * time.Second,
		SearchDepth:   "basic",
		IncludeAnswer: false,
	}
}

// tavilyProvider implements WebSearchProvider using Tavily Search API
type tavilyProvider struct {
	config TavilyConfig
	client *http.Client
}

// NewTavilyProvider creates a Tavily search provider
func NewTavilyProvider(config TavilyConfig) (WebSearchProvider, error) {
	if config.APIKey == "" {
		return nil, fmt.Errorf("tavily: api_key is required")
	}
	if config.MaxResults <= 0 {
		config.MaxResults = 10
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.SearchDepth == "" {
		config.SearchDepth = "basic"
	}

	return &tavilyProvider{
		config: config,
		client: &http.Client{Timeout: config.Timeout},
	}, nil
}

func (p *tavilyProvider) Name() string { return "tavily" }

// tavilySearchRequest is the request body for Tavily Search API
type tavilySearchRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results,omitempty"`
	SearchDepth   string `json:"search_depth,omitempty"`
	IncludeAnswer bool   `json:"include_answer,omitempty"`
}

// tavilySearchResponse is the response from Tavily Search API
type tavilySearchResponse struct {
	Query   string               `json:"query"`
	Answer  string               `json:"answer,omitempty"`
	Results []tavilySearchResult `json:"results"`
}

// tavilySearchResult is a single result from Tavily
type tavilySearchResult struct {
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	RawContent string  `json:"raw_content,omitempty"`
}

func (p *tavilyProvider) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	start := time.Now()

	// Get proxy from context if available
	var client *http.Client
	if proxy := ProxyFromContext(ctx); proxy != nil {
		var err error
		client, err = createHTTPClient(proxy)
		if err != nil {
			return nil, fmt.Errorf("create proxy client: %w", err)
		}
		defer client.CloseIdleConnections()
	} else {
		client = p.client
	}

	// Build request
	maxResults := p.config.MaxResults
	if req.MaxResults > 0 && req.MaxResults < maxResults {
		maxResults = req.MaxResults
	}

	tavilyReq := tavilySearchRequest{
		APIKey:        p.config.APIKey,
		Query:         req.Query,
		MaxResults:    maxResults,
		SearchDepth:   p.config.SearchDepth,
		IncludeAnswer: p.config.IncludeAnswer,
	}

	body, err := json.Marshal(tavilyReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	// Execute request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tavily API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var tavilyResp tavilySearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&tavilyResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	// Convert to SearchResult
	results := make([]SearchResult, 0, len(tavilyResp.Results))
	for _, r := range tavilyResp.Results {
		results = append(results, SearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Content,
			Source:      "tavily",
		})
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("tavily returned 0 results")
	}

	return &SearchResponse{
		Results:    results,
		TotalCount: len(results),
		Provider:   p.Name(),
		Duration:   time.Since(start),
	}, nil
}

func (p *tavilyProvider) HealthCheck(ctx context.Context) error {
	// Simple health check: search for a common term
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", nil)
	if err != nil {
		return err
	}

	body, _ := json.Marshal(tavilySearchRequest{
		APIKey:     p.config.APIKey,
		Query:      "test",
		MaxResults: 1,
	})
	req, _ = http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}

	return nil
}

func (p *tavilyProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
