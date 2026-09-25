package webfetch

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// FetchMetrics holds all Prometheus metrics for the webfetch subsystem.
type FetchMetrics struct {
	fetchTotal           *prometheus.CounterVec   // labels: status
	fetchErrors          *prometheus.CounterVec   // labels: error_type
	fetchDuration        *prometheus.HistogramVec // labels: domain_type
	cacheHits            prometheus.Counter
	cacheMisses          prometheus.Counter
	concurrencyCurrent   prometheus.Gauge
	concurrencyLimit     *prometheus.CounterVec // labels: type (global/domain)
	llmExtractTotal      *prometheus.CounterVec // labels: status
	llmExtractDuration   *prometheus.HistogramVec
	contentSize          *prometheus.HistogramVec // labels: content_type
	redirectTotal        *prometheus.CounterVec   // labels: type
	ssrfBlocked          prometheus.Counter
}

var (
	fetchMetrics     *FetchMetrics
	fetchMetricsOnce sync.Once
)

// GetFetchMetrics returns the global FetchMetrics singleton.
func GetFetchMetrics() *FetchMetrics {
	fetchMetricsOnce.Do(func() {
		fetchMetrics = newFetchMetrics()
	})
	return fetchMetrics
}

func newFetchMetrics() *FetchMetrics {
	return &FetchMetrics{
		fetchTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_requests_total",
			Help: "Total number of fetch requests",
		}, []string{"status"}),
		fetchErrors: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_errors_total",
			Help: "Total number of fetch errors",
		}, []string{"error_type"}),
		fetchDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rtc_agent_webfetch_duration_seconds",
			Help:    "Fetch request duration in seconds",
			Buckets: prometheus.DefBuckets,
		}, []string{"domain_type"}),
		cacheHits: promauto.NewCounter(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_cache_hits_total",
			Help: "Total number of cache hits",
		}),
		cacheMisses: promauto.NewCounter(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_cache_misses_total",
			Help: "Total number of cache misses",
		}),
		concurrencyCurrent: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rtc_agent_webfetch_concurrency_current",
			Help: "Current number of concurrent fetches",
		}),
		concurrencyLimit: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_concurrency_limit_total",
			Help: "Total number of concurrency limit hits",
		}, []string{"type"}),
		llmExtractTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_llm_extract_total",
			Help: "Total number of LLM extraction calls",
		}, []string{"status"}),
		llmExtractDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rtc_agent_webfetch_llm_extract_duration_seconds",
			Help:    "LLM extraction duration in seconds",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60},
		}, nil),
		contentSize: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rtc_agent_webfetch_content_size_bytes",
			Help:    "Content size distribution",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 8),
		}, []string{"content_type"}),
		redirectTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_redirect_total",
			Help: "Total number of redirects",
		}, []string{"type"}),
		ssrfBlocked: promauto.NewCounter(prometheus.CounterOpts{
			Name: "rtc_agent_webfetch_ssrf_blocked_total",
			Help: "Total number of SSRF blocks",
		}),
	}
}

// Convenience recording methods.

func (m *FetchMetrics) RecordSuccess(domainType string, durationMs int64) {
	m.fetchTotal.WithLabelValues("success").Inc()
	m.fetchDuration.WithLabelValues(domainType).Observe(float64(durationMs) / 1000)
}

func (m *FetchMetrics) RecordError(errorType string) {
	m.fetchTotal.WithLabelValues("error").Inc()
	m.fetchErrors.WithLabelValues(errorType).Inc()
}

func (m *FetchMetrics) RecordCacheHit()  { m.cacheHits.Inc() }
func (m *FetchMetrics) RecordCacheMiss() { m.cacheMisses.Inc() }

func (m *FetchMetrics) RecordSSRFBlocked() {
	m.ssrfBlocked.Inc()
	m.fetchTotal.WithLabelValues("blocked").Inc()
}

func (m *FetchMetrics) RecordConcurrencyLimit(t string) {
	m.concurrencyLimit.WithLabelValues(t).Inc()
}

func (m *FetchMetrics) RecordRateLimited() {
	m.fetchTotal.WithLabelValues("rate_limited").Inc()
}
