package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// HTTP server Prometheus metrics, aligned with grafana/dashboards/http-server.json panels.
var (
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"method", "handler", "code"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 20), // 5ms ~ 4369s
		},
		[]string{"handler"},
	)

	httpRequestSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "http_request_size_bytes",
			Help:    "HTTP request size in bytes.",
			Buckets: prometheus.ExponentialBuckets(100, 10, 7), // 100B ~ 100MB
		},
	)

	httpResponseSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "HTTP response size in bytes.",
			Buckets: prometheus.ExponentialBuckets(100, 10, 7), // 100B ~ 100MB
		},
	)

	httpInFlightRequests = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_in_flight_requests",
			Help: "Number of HTTP requests currently being served.",
		},
	)
)

// HTTPMetrics returns a Prometheus HTTP metrics middleware.
//
// Uses github.com/felixge/httpsnoop to transparently wrap http.ResponseWriter,
// ensuring all ResponseWriter interfaces (http.Hijacker, http.Flusher, http.Pusher, etc.)
// are correctly preserved, avoiding WebSocket upgrade or streaming response failures due to interface assertion.
//
// Captured metrics:
//   - http_requests_total          request count (grouped by method/handler/code)
//   - http_request_duration_seconds request latency distribution
//   - http_request_size_bytes       request body size distribution
//   - http_response_size_bytes      response body size distribution
//   - http_in_flight_requests       concurrent request count
//
// Skips /healthz, /metrics, and WebSocket upgrade requests to avoid noise.
func HTTPMetrics() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip metrics recording for /healthz and /metrics to avoid noise.
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			// Skip WebSocket upgrade requests (long-lived connections, should not be counted in HTTP metrics).
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				next.ServeHTTP(w, r)
				return
			}

			// In-flight: +1 on start, -1 on completion.
			httpInFlightRequests.Inc()
			defer httpInFlightRequests.Dec()

			// Request size.
			if r.ContentLength > 0 {
				httpRequestSize.Observe(float64(r.ContentLength))
			}

			start := time.Now()

			// Use httpsnoop to transparently wrap ResponseWriter,
			// preserving Hijacker/Flusher/Pusher and all other interfaces.
			wrapped := httpsnoop.CaptureMetrics(next, w, r)

			code := wrapped.Code
			written := wrapped.Written
			duration := time.Since(start).Seconds()
			handler := r.URL.Path
			codeStr := strconv.Itoa(code)

			// Request count.
			httpRequestsTotal.WithLabelValues(r.Method, handler, codeStr).Inc()

			// Latency.
			httpRequestDuration.WithLabelValues(handler).Observe(duration)

			// Response size.
			if written > 0 {
				httpResponseSize.Observe(float64(written))
			}
		})
	}
}
