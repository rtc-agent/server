package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SearXNGConfig defines SearXNG provider configuration
type SearXNGConfig struct {
	BaseURL    string        `json:"base_url"`    // e.g., http://localhost:8080
	MaxResults int           `json:"max_results"` // Default: 10
	Timeout    time.Duration `json:"timeout"`     // Default: 30s
	Language   string        `json:"language"`    // e.g., "en", "zh"
}

// DefaultSearXNGConfig returns sensible defaults
func DefaultSearXNGConfig() SearXNGConfig {
	return SearXNGConfig{
		BaseURL:    "http://localhost:8080",
		MaxResults: 10,
		Timeout:    30 * time.Second,
		Language:   "en",
	}
}

// searxngResponse represents SearXNG JSON API response
type searxngResponse struct {
	Query   string `json:"query"`
	Results []struct {
		URL     string  `json:"url"`
		Title   string  `json:"title"`
		Content string  `json:"content"`
		Engine  string  `json:"engine"`
		Score   float64 `json:"score"`
	} `json:"results"`
	NumberOfResults int `json:"number_of_results"`
}

// searxngProvider implements WebSearchProvider using SearXNG meta-search engine
type searxngProvider struct {
	config SearXNGConfig
	client *http.Client
}

// NewSearXNGProvider creates a SearXNG search provider
func NewSearXNGProvider(config SearXNGConfig) (WebSearchProvider, error) {
	if config.BaseURL == "" {
		return nil, fmt.Errorf("SearXNG base URL is required")
	}

	// Validate and normalize BaseURL
	u, err := url.Parse(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid SearXNG base URL: %w", err)
	}
	config.BaseURL = strings.TrimRight(u.String(), "/")

	if config.MaxResults <= 0 {
		config.MaxResults = 10
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Language == "" {
		config.Language = "en"
	}

	return &searxngProvider{
		config: config,
		client: &http.Client{Timeout: config.Timeout},
	}, nil
}

func (p *searxngProvider) Name() string { return "searxng" }

func (p *searxngProvider) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
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

	// Build query parameters
	params := url.Values{}
	params.Set("q", req.Query)
	params.Set("format", "json")
	params.Set("language", p.config.Language)

	if req.MaxResults > 0 {
		params.Set("pageno", "1")
	}

	if req.TimeRange != "" {
		params.Set("time_range", string(req.TimeRange))
	}

	searchURL := fmt.Sprintf("%s/search?%s", p.config.BaseURL, params.Encode())

	// Create request
	httpReq, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Accept", "application/json")

	// Execute request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("searxng error (status %d): %s", resp.StatusCode, string(body))
	}

	// Parse response
	var searxngResp searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&searxngResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Convert to SearchResponse
	results := make([]SearchResult, 0, len(searxngResp.Results))
	for _, r := range searxngResp.Results {
		if len(results) >= p.config.MaxResults {
			break
		}
		results = append(results, SearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Content,
			Source:      r.Engine,
			Score:       r.Score,
		})
	}

	return &SearchResponse{
		Results:    results,
		TotalCount: len(results),
		Provider:   p.Name(),
		Duration:   time.Since(start),
	}, nil
}

func (p *searxngProvider) HealthCheck(ctx context.Context) error {
	// Health check: ping SearXNG instance with minimal request
	healthURL := fmt.Sprintf("%s/search?q=test&format=json", p.config.BaseURL)

	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Consume body to enable connection reuse
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}

	return nil
}

func (p *searxngProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
