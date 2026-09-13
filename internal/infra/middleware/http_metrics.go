package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

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

// metricsResponseWriter 包装 http.ResponseWriter 以捕获状态码和写入字节数。
type metricsResponseWriter struct {
	http.ResponseWriter
	code    int
	written int
}

func (w *metricsResponseWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *metricsResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	return n, err
}

// HTTPMetrics 返回一个 Prometheus HTTP 指标中间件。
// 自定义实现（不依赖 promhttp.InstrumentHandler*），避免 label 校验限制。
// 指标名与 grafana/dashboards/http-server.json 面板对齐。
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
			rw := &metricsResponseWriter{ResponseWriter: w, code: http.StatusOK}

			next.ServeHTTP(rw, r)

			duration := time.Since(start).Seconds()
			handler := r.URL.Path
			code := strconv.Itoa(rw.code)

			// 请求计数
			httpRequestsTotal.WithLabelValues(r.Method, handler, code).Inc()

			// 延迟
			httpRequestDuration.WithLabelValues(handler).Observe(duration)

			// 响应大小
			if rw.written > 0 {
				httpResponseSize.Observe(float64(rw.written))
			}
		})
	}
}
