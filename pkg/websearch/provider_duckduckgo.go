package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/leichujun/rtc-agent/server/pkg/proxy"
)

// DuckDuckGo JSON API endpoint (same as eino-ext)
const duckDuckGoSearchURL = "https://links.duckduckgo.com/d.js"

// DuckDuckGoConfig defines DuckDuckGo provider configuration
type DuckDuckGoConfig struct {
	MaxResults int           `json:"max_results"` // Default: 10
	Timeout    time.Duration `json:"timeout"`     // Default: 30s
	Region     string        `json:"region"`      // e.g., "us-en", "wt-wt"
}

// DefaultDuckDuckGoConfig returns sensible defaults
func DefaultDuckDuckGoConfig() DuckDuckGoConfig {
	return DuckDuckGoConfig{
		MaxResults: 10,
		Timeout:    30 * time.Second,
		Region:     "wt-wt",
	}
}

// duckDuckGoProvider implements WebSearchProvider using DuckDuckGo JSON API
type duckDuckGoProvider struct {
	config DuckDuckGoConfig
	client *http.Client
}

// NewDuckDuckGoProvider creates a DuckDuckGo search provider
func NewDuckDuckGoProvider(config DuckDuckGoConfig) (WebSearchProvider, error) {
	if config.MaxResults <= 0 {
		config.MaxResults = 10
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Region == "" {
		config.Region = "wt-wt"
	}

	return &duckDuckGoProvider{
		config: config,
		client: &http.Client{Timeout: config.Timeout},
	}, nil
}

func (p *duckDuckGoProvider) Name() string { return "duckduckgo" }

// ddgAPIResponse represents the JSON response from DuckDuckGo API
type ddgAPIResponse struct {
	Results []struct {
		Title       string `json:"t"`
		URL         string `json:"u"`
		Description string `json:"a"`
	} `json:"results"`
	NoResults bool `json:"noResults"`
}

func (p *duckDuckGoProvider) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
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

	// Step 1: Get VQD token (required by DuckDuckGo API)
	vqd, err := p.getVQD(ctx, client, req.Query)
	if err != nil {
		return nil, fmt.Errorf("get vqd token: %w", err)
	}

	// Step 2: Build search API URL
	params := url.Values{}
	params.Set("q", req.Query)
	params.Set("vqd", vqd)
	params.Set("o", "json")           // Output format: JSON
	params.Set("kl", p.config.Region) // Region
	params.Set("ss", "1")            // Show snippets
	params.Set("sp", "1")            // Show preference cookies
	params.Set("sc", "1")            // Show category headers

	searchURL := duckDuckGoSearchURL + "?" + params.Encode()

	// Step 3: Execute API request
	httpReq, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set headers to mimic browser
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Step 4: Parse JSON response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var apiResp ddgAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	if apiResp.NoResults {
		return &SearchResponse{
			Results:    nil,
			TotalCount: 0,
			Provider:   p.Name(),
			Duration:   time.Since(start),
		}, nil
	}

	// Step 5: Convert API results to SearchResult
	maxResults := p.config.MaxResults
	if req.MaxResults > 0 && req.MaxResults < maxResults {
		maxResults = req.MaxResults
	}

	var results []SearchResult
	for i, r := range apiResp.Results {
		if i >= maxResults {
			break
		}
		// Skip empty results
		if r.Title == "" && r.URL == "" && r.Description == "" {
			continue
		}
		results = append(results, SearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Description,
			Source:      "duckduckgo",
		})
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("duckduckgo returned 0 results")
	}

	return &SearchResponse{
		Results:    results,
		TotalCount: len(results),
		Provider:   p.Name(),
		Duration:   time.Since(start),
	}, nil
}

// getVQD retrieves the VQD token required for DuckDuckGo API search requests.
// The VQD token is a verification token that DuckDuckGo requires for all API calls.
func (p *duckDuckGoProvider) getVQD(ctx context.Context, client *http.Client, query string) (string, error) {
	// Visit DuckDuckGo homepage with query to get VQD token
	endpoint := "https://duckduckgo.com"

	httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	// Add query parameter
	q := httpReq.URL.Query()
	q.Set("q", query)
	httpReq.URL.RawQuery = q.Encode()

	// Set headers
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	// Extract VQD token from HTML using regex
	vqd := extractVQDToken(string(body))
	if vqd == "" {
		return "", fmt.Errorf("failed to extract VQD token")
	}

	return vqd, nil
}

// extractVQDToken extracts the VQD token from DuckDuckGo HTML response
func extractVQDToken(html string) string {
	re := regexp.MustCompile(`vqd=["']([^"']+)["']`)
	matches := re.FindStringSubmatch(html)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func (p *duckDuckGoProvider) HealthCheck(ctx context.Context) error {
	// Use HEAD request for lightweight health check
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://duckduckgo.com", nil)
	if err != nil {
		return err
	}

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

func (p *duckDuckGoProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
