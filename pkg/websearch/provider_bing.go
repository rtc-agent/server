package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/proxy"
)

// BingConfig defines Bing API provider configuration
type BingConfig struct {
	APIKey     string        `json:"api_key"`     // Required: Bing Search API key
	Endpoint   string        `json:"endpoint"`    // Default: https://api.bing.microsoft.com/v7.0/search
	MaxResults int           `json:"max_results"` // Default: 10
	Timeout    time.Duration `json:"timeout"`     // Default: 30s
	Market     string        `json:"market"`      // e.g., "en-US", "zh-CN"
}

// DefaultBingConfig returns sensible defaults
func DefaultBingConfig() BingConfig {
	return BingConfig{
		Endpoint:   "https://api.bing.microsoft.com/v7.0/search",
		MaxResults: 10,
		Timeout:    30 * time.Second,
		Market:     "en-US",
	}
}

// bingResponse represents Bing API response structure
type bingResponse struct {
	WebPages struct {
		Value []bingWebPage `json:"value"`
	} `json:"webPages"`
}

type bingWebPage struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	DateLastCrawled string `json:"dateLastCrawled"`
}

// bingProvider implements WebSearchProvider using Bing Search API
type bingProvider struct {
	config BingConfig
	client *http.Client
}

// NewBingProvider creates a Bing search provider
func NewBingProvider(config BingConfig) (WebSearchProvider, error) {
	if config.APIKey == "" {
		return nil, fmt.Errorf("bing API key is required")
	}
	if config.Endpoint == "" {
		config.Endpoint = "https://api.bing.microsoft.com/v7.0/search"
	}
	if config.MaxResults <= 0 {
		config.MaxResults = 10
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Market == "" {
		config.Market = "en-US"
	}

	return &bingProvider{
		config: config,
		client: &http.Client{Timeout: config.Timeout},
	}, nil
}

func (p *bingProvider) Name() string { return "bing" }

func (p *bingProvider) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	start := time.Now()

	// Get proxy from context if available
	var client *http.Client
	if px := ProxyFromContext(ctx); px != nil {
		var err error
		client, err = proxy.CreateHTTPClient(px)
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
	params.Set("count", fmt.Sprintf("%d", p.config.MaxResults))
	params.Set("mkt", p.config.Market)
	params.Set("safeSearch", "Moderate")

	if req.TimeRange != "" {
		// Bing uses freshness parameter for time filtering
		switch req.TimeRange {
		case TimeRangeDay:
			params.Set("freshness", "Day")
		case TimeRangeWeek:
			params.Set("freshness", "Week")
		case TimeRangeMonth:
			params.Set("freshness", "Month")
		}
	}

	searchURL := fmt.Sprintf("%s?%s", p.config.Endpoint, params.Encode())

	// Create request
	httpReq, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Ocp-Apim-Subscription-Key", p.config.APIKey)
	httpReq.Header.Set("Accept", "application/json")

	// Execute request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("bing API error (status %d): %s", resp.StatusCode, string(body))
	}

	// Parse response
	var bingResp bingResponse
	if err := json.NewDecoder(resp.Body).Decode(&bingResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Convert to SearchResponse
	results := make([]SearchResult, 0, len(bingResp.WebPages.Value))
	for _, page := range bingResp.WebPages.Value {
		results = append(results, SearchResult{
			Title:       page.Name,
			URL:         page.URL,
			Description: page.Snippet,
			Source:      "bing",
		})
	}

	return &SearchResponse{
		Results:    results,
		TotalCount: len(results),
		Provider:   p.Name(),
		Duration:   time.Since(start),
	}, nil
}

func (p *bingProvider) HealthCheck(ctx context.Context) error {
	// Health check: verify API key is valid without consuming quota
	// Use a minimal request with count=0 to avoid result charges
	req, err := http.NewRequestWithContext(ctx, "GET", p.config.Endpoint+"?q=test&count=0", nil)
	if err != nil {
		return err
	}

	req.Header.Set("Ocp-Apim-Subscription-Key", p.config.APIKey)
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

func (p *bingProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
