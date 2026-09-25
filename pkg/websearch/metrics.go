package websearch

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics defines Prometheus metrics for web search
type Metrics struct {
	// Request counters
	searchTotal    *prometheus.CounterVec
	searchErrors   *prometheus.CounterVec
	searchDuration *prometheus.HistogramVec

	// Provider metrics
	providerHealth *prometheus.GaugeVec
	circuitState   *prometheus.GaugeVec

	// Proxy metrics
	proxyHealth    *prometheus.GaugeVec
	proxyLatency   *prometheus.GaugeVec
	proxyAvailable *prometheus.GaugeVec

	// Cache metrics
	cacheHits   prometheus.Counter
	cacheMisses prometheus.Counter

	// Rate limiter metrics
	rateLimited *prometheus.CounterVec
}

// Global metrics instance (lazy initialized)
var (
	metrics     *Metrics
	metricsOnce sync.Once
)

// GetMetrics returns the global metrics instance
func GetMetrics() *Metrics {
	metricsOnce.Do(func() {
		metrics = newMetrics()
	})
	return metrics
}

// newMetrics creates and registers Prometheus metrics
func newMetrics() *Metrics {
	m := &Metrics{
		// Request counters
		searchTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rtc_agent_websearch_requests_total",
				Help: "Total number of search requests",
			},
			[]string{"provider", "status"},
		),
		searchErrors: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rtc_agent_websearch_errors_total",
				Help: "Total number of search errors",
			},
			[]string{"provider", "error_type"},
		),
		searchDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rtc_agent_websearch_duration_seconds",
				Help:    "Search request duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"provider"},
		),

		// Provider metrics
		providerHealth: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "rtc_agent_websearch_provider_health",
				Help: "Provider health status (1=healthy, 0=unhealthy)",
			},
			[]string{"provider"},
		),
		circuitState: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "rtc_agent_websearch_circuit_state",
				Help: "Circuit breaker state (0=closed, 1=half_open, 2=open)",
			},
			[]string{"provider"},
		),

		// Proxy metrics
		proxyHealth: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "rtc_agent_websearch_proxy_health",
				Help: "Proxy health status (1=healthy, 0=unhealthy)",
			},
			[]string{"proxy_url"},
		),
		proxyLatency: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "rtc_agent_websearch_proxy_latency_seconds",
				Help: "Proxy average latency in seconds",
			},
			[]string{"proxy_url"},
		),
		proxyAvailable: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "rtc_agent_websearch_proxy_available",
				Help: "Number of available healthy proxies",
			},
			[]string{},
		),

		// Cache metrics
		cacheHits: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "rtc_agent_websearch_cache_hits_total",
				Help: "Total number of cache hits",
			},
		),
		cacheMisses: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "rtc_agent_websearch_cache_misses_total",
				Help: "Total number of cache misses",
			},
		),

		// Rate limiter metrics
		rateLimited: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rtc_agent_websearch_rate_limited_total",
				Help: "Total number of rate limited requests",
			},
			[]string{"provider"},
		),
	}

	return m
}

// RecordSearch records a search request
func (m *Metrics) RecordSearch(provider, status string, duration time.Duration) {
	m.searchTotal.WithLabelValues(provider, status).Inc()
	m.searchDuration.WithLabelValues(provider).Observe(duration.Seconds())
}

// RecordError records a search error
func (m *Metrics) RecordError(provider, errorType string) {
	m.searchErrors.WithLabelValues(provider, errorType).Inc()
}

// RecordProviderHealth records provider health status
func (m *Metrics) RecordProviderHealth(provider string, healthy bool) {
	value := 0.0
	if healthy {
		value = 1.0
	}
	m.providerHealth.WithLabelValues(provider).Set(value)
}

// RecordCircuitState records circuit breaker state
func (m *Metrics) RecordCircuitState(provider string, state CircuitState) {
	m.circuitState.WithLabelValues(provider).Set(float64(state))
}

// RecordProxyHealth records proxy health metrics
func (m *Metrics) RecordProxyHealth(proxyURL string, healthy bool, latency time.Duration) {
	healthValue := 0.0
	if healthy {
		healthValue = 1.0
	}
	m.proxyHealth.WithLabelValues(proxyURL).Set(healthValue)
	m.proxyLatency.WithLabelValues(proxyURL).Set(latency.Seconds())
}

// RecordProxyAvailable records number of available proxies
func (m *Metrics) RecordProxyAvailable(count int) {
	m.proxyAvailable.WithLabelValues().Set(float64(count))
}

// RecordCacheHit records a cache hit
func (m *Metrics) RecordCacheHit() {
	m.cacheHits.Inc()
}

// RecordCacheMiss records a cache miss
func (m *Metrics) RecordCacheMiss() {
	m.cacheMisses.Inc()
}

// RecordRateLimited records a rate limited request
func (m *Metrics) RecordRateLimited(provider string) {
	m.rateLimited.WithLabelValues(provider).Inc()
}

// SearchStats tracks search statistics for monitoring
type SearchStats struct {
	TotalRequests   atomic.Int64
	SuccessRequests atomic.Int64
	FailedRequests  atomic.Int64
	TotalDuration   atomic.Int64 // nanoseconds

	// Per-provider stats (simplified, could be expanded)
	ProviderRequests map[string]*atomic.Int64
	ProviderFailures map[string]*atomic.Int64
	mu               sync.RWMutex
}

// NewSearchStats creates a new stats tracker
func NewSearchStats() *SearchStats {
	return &SearchStats{
		ProviderRequests: make(map[string]*atomic.Int64),
		ProviderFailures: make(map[string]*atomic.Int64),
	}
}

// RecordSuccess records a successful search
func (s *SearchStats) RecordSuccess(provider string, duration time.Duration) {
	s.TotalRequests.Add(1)
	s.SuccessRequests.Add(1)
	s.TotalDuration.Add(int64(duration))

	s.mu.RLock()
	counter, ok := s.ProviderRequests[provider]
	s.mu.RUnlock()

	if !ok {
		s.mu.Lock()
		counter, ok = s.ProviderRequests[provider]
		if !ok {
			counter = &atomic.Int64{}
			s.ProviderRequests[provider] = counter
		}
		s.mu.Unlock()
	}
	counter.Add(1)
}

// RecordFailure records a failed search
func (s *SearchStats) RecordFailure(provider string) {
	s.TotalRequests.Add(1)
	s.FailedRequests.Add(1)

	s.mu.RLock()
	counter, ok := s.ProviderFailures[provider]
	s.mu.RUnlock()

	if !ok {
		s.mu.Lock()
		counter, ok = s.ProviderFailures[provider]
		if !ok {
			counter = &atomic.Int64{}
			s.ProviderFailures[provider] = counter
		}
		s.mu.Unlock()
	}
	counter.Add(1)
}

// GetStats returns current statistics
func (s *SearchStats) GetStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	providerStats := make(map[string]map[string]int64)
	for provider, counter := range s.ProviderRequests {
		if providerStats[provider] == nil {
			providerStats[provider] = make(map[string]int64)
		}
		providerStats[provider]["requests"] = counter.Load()
	}
	for provider, counter := range s.ProviderFailures {
		if providerStats[provider] == nil {
			providerStats[provider] = make(map[string]int64)
		}
		providerStats[provider]["failures"] = counter.Load()
	}

	totalDuration := time.Duration(s.TotalDuration.Load())
	totalRequests := s.TotalRequests.Load()
	var avgDuration time.Duration
	if totalRequests > 0 {
		avgDuration = totalDuration / time.Duration(totalRequests)
	}

	return map[string]interface{}{
		"total_requests":   totalRequests,
		"success_requests": s.SuccessRequests.Load(),
		"failed_requests":  s.FailedRequests.Load(),
		"total_duration":   totalDuration.String(),
		"avg_duration":     avgDuration.String(),
		"providers":        providerStats,
	}
}
