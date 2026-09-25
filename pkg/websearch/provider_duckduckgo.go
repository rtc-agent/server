package websearch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// DuckDuckGoConfig defines DuckDuckGo provider configuration
type DuckDuckGoConfig struct {
	MaxResults int           `json:"max_results"` // Default: 10
	Timeout    time.Duration `json:"timeout"`     // Default: 30s
	Region     string        `json:"region"`      // e.g., "us-en", "cn-zh"
}

// DefaultDuckDuckGoConfig returns sensible defaults
func DefaultDuckDuckGoConfig() DuckDuckGoConfig {
	return DuckDuckGoConfig{
		MaxResults: 10,
		Timeout:    30 * time.Second,
		Region:     "us-en",
	}
}

// duckDuckGoProvider implements WebSearchProvider using DuckDuckGo HTML scraping
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
		config.Region = "us-en"
	}

	return &duckDuckGoProvider{
		config: config,
		client: &http.Client{Timeout: config.Timeout},
	}, nil
}

func (p *duckDuckGoProvider) Name() string { return "duckduckgo" }

func (p *duckDuckGoProvider) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
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

	// Build DuckDuckGo HTML search URL
	params := url.Values{}
	params.Set("q", req.Query)
	params.Set("kl", p.config.Region)

	searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?%s", params.Encode())

	// Create request
	httpReq, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set headers to mimic browser
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	// Execute request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Parse HTML response
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	// Extract search results
	var results []SearchResult
	doc.Find("div.result").Each(func(i int, s *goquery.Selection) {
		if i >= p.config.MaxResults {
			return
		}

		// Extract title and URL
		titleSel := s.Find("h2.result__title a.result__url")
		title := titleSel.Text()
		resultURL, _ := titleSel.Attr("href")

		// Extract snippet
		snippet := s.Find("a.result__snippet").Text()

		// Clean up URL (DuckDuckGo uses redirect URLs like /l/?uddg=https%3A%2F%2F...)
		if resultURL != "" {
			if parsedURL, err := url.Parse(resultURL); err == nil {
				if uddg := parsedURL.Query().Get("uddg"); uddg != "" {
					resultURL = uddg
				}
			}

			results = append(results, SearchResult{
				Title:       title,
				URL:         resultURL,
				Description: snippet,
				Source:      "duckduckgo",
			})
		}
	})

	// Warn if no results parsed (HTML structure may have changed)
	if len(results) == 0 {
		return nil, fmt.Errorf("duckduckgo returned 0 results (HTML structure may have changed)")
	}

	return &SearchResponse{
		Results:    results,
		TotalCount: len(results),
		Provider:   p.Name(),
		Duration:   time.Since(start),
	}, nil
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
