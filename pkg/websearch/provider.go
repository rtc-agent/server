// Package websearch provides a high-availability, load-balanced web search tool
// with circuit breaking, rate limiting, and proxy support.
package websearch

import (
	"context"
	"errors"
	"time"
)

// WebSearchProvider defines the unified interface for all search engine implementations.
// Each provider must implement Search, HealthCheck, and Close methods.
type WebSearchProvider interface {
	// Name returns the provider name (used for logging and metrics)
	Name() string

	// Search executes a search query
	// Proxy is injected via context (proxy.FromContext(ctx)), provider decides whether to use it
	Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error)

	// HealthCheck checks service health status
	HealthCheck(ctx context.Context) error

	// Close releases provider resources
	Close() error
}

// SearchRequest represents a search query request
type SearchRequest struct {
	Query      string    // Search query
	MaxResults int       // Maximum number of results
	TimeRange  TimeRange // Time range filter
	Language   string    // Language code (e.g., "en", "zh")
	Region     string    // Region code (e.g., "us", "cn")
	SafeSearch bool      // Enable safe search
}

// SearchResponse represents search results
type SearchResponse struct {
	Results    []SearchResult // Search results
	TotalCount int            // Total result count
	Provider   string         // Provider that returned results
	Duration   time.Duration  // Request duration
	Cached     bool           // Whether from cache
}

// SearchResult represents a single search result
type SearchResult struct {
	Title       string  // Result title
	URL         string  // Result URL
	Description string  // Result description/snippet
	Source      string  // Source engine (Google, Bing, etc.)
	Score       float64 // Quality score (0-1, for multi-provider ranking)
}

// TimeRange defines time-based filtering
type TimeRange string

const (
	TimeRangeDay   TimeRange = "day"
	TimeRangeWeek  TimeRange = "week"
	TimeRangeMonth TimeRange = "month"
	TimeRangeYear  TimeRange = "year"
)

// ErrNoProvider is returned when no search provider is available
var ErrNoProvider = errors.New("no available web search provider")
