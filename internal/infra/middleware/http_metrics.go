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

// HTTP 服务器 Prometheus 指标，与 grafana/dashboards/http-server.json 面板对齐。
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

// HTTPMetrics 返回一个 Prometheus HTTP 指标中间件。
//
// 使用 github.com/felixge/httpsnoop 透明包装 http.ResponseWriter，
// 确保所有 ResponseWriter 接口（http.Hijacker、http.Flusher、http.Pusher 等）
// 都被正确保留，避免 WebSocket 升级或流式响应因接口断言失败。
//
// 捕获的指标：
//   - http_requests_total        请求计数（按 method/handler/code 分组）
//   - http_request_duration_seconds 请求延迟分布
//   - http_request_size_bytes    请求体大小分布
//   - http_response_size_bytes   响应体大小分布
//   - http_in_flight_requests    并发请求数
//
// 跳过 /healthz、/metrics 和 WebSocket 升级请求，避免噪音。
func HTTPMetrics() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 跳过 /healthz 和 /metrics 的指标记录，避免噪音
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			// 跳过 WebSocket 升级请求（长连接，不应计入 HTTP 指标）
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				next.ServeHTTP(w, r)
				return
			}

			// In-flight: 请求开始 +1，结束 -1
			httpInFlightRequests.Inc()
			defer httpInFlightRequests.Dec()

			// 请求大小
			if r.ContentLength > 0 {
				httpRequestSize.Observe(float64(r.ContentLength))
			}

			start := time.Now()

			// 使用 httpsnoop 透明包装 ResponseWriter，
			// 保留 Hijacker/Flusher/Pusher 等所有接口。
			wrapped := httpsnoop.CaptureMetrics(next, w, r)

			code := wrapped.Code
			written := wrapped.Written
			duration := time.Since(start).Seconds()
			handler := r.URL.Path
			codeStr := strconv.Itoa(code)

			// 请求计数
			httpRequestsTotal.WithLabelValues(r.Method, handler, codeStr).Inc()

			// 延迟
			httpRequestDuration.WithLabelValues(handler).Observe(duration)

			// 响应大小
			if written > 0 {
				httpResponseSize.Observe(float64(written))
			}
		})
	}
}
