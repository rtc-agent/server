// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// MetricsProxyHandler reverse-proxies Prometheus API requests.
// Requests to /api/metrics/* are forwarded to Prometheus /api/v1/*.
// If prometheusURL is empty, all requests return 501 Not Implemented.
type MetricsProxyHandler struct {
	proxy *httputil.ReverseProxy
}

// NewMetricsProxyHandler creates a new MetricsProxyHandler.
// If prometheusURL is empty, the handler returns 501 for all requests.
func NewMetricsProxyHandler(prometheusURL string) *MetricsProxyHandler {
	if prometheusURL == "" {
		logger.Warn(context.Background(), "admin.prometheus_url_not_configured_metrics_proxy_disabled")
		return &MetricsProxyHandler{}
	}

	target, err := url.Parse(prometheusURL)
	if err != nil {
		logger.Error(context.Background(), "admin.prometheus_url_parse_failed_metrics_proxy_disabled",
			zap.String("url", prometheusURL), zap.Error(err))
		return &MetricsProxyHandler{}
	}

	// Construct ReverseProxy directly (avoid NewSingleHostReverseProxy which sets
	// deprecated Director). Go requires exactly one of Director or Rewrite.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Strip /api/metrics prefix and replace with /api/v1
			r.Out.URL.Path = rewritePath(r.Out.URL.Path)
			r.Out.URL.RawPath = "" // clear RawPath so URL.Path is used
		},
		Transport: sharedProxyTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error(r.Context(), "admin.metrics_proxy_error",
				zap.String("path", r.URL.Path), zap.Error(err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_gateway","error":"prometheus unavailable"}`))
		},
	}

	logger.Info(context.Background(), "admin.metrics_proxy_enabled",
		zap.String("prometheus_url", prometheusURL))

	return &MetricsProxyHandler{proxy: proxy}
}

// rewritePath converts /api/metrics/<endpoint> to /api/v1/<endpoint>.
func rewritePath(path string) string {
	// Expected: /api/metrics/query → /api/v1/query
	// /api/metrics/query_range → /api/v1/query_range
	const prefix = "/api/metrics"
	if strings.HasPrefix(path, prefix) {
		rest := path[len(prefix):]
		return "/api/v1" + rest
	}
	return path
}

// RegisterRoutes registers the metrics proxy routes under the given router group.
// Routes are registered under /metrics/* to match the apiGroup prefix (/api).
func (h *MetricsProxyHandler) RegisterRoutes(r *gin.RouterGroup) {
	// Use Any to support all HTTP methods (Prometheus uses GET for queries,
	// but POST for some endpoints like /api/v1/query with large payloads)
	r.Any("/metrics/*path", h.handleProxy)
}

// handleProxy forwards the request to Prometheus or returns 501 if not configured.
func (h *MetricsProxyHandler) handleProxy(c *gin.Context) {
	if h.proxy == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"status":    "error",
			"errorType": "not_implemented",
			"error":     "metrics proxy not configured",
		})
		return
	}
	h.proxy.ServeHTTP(c.Writer, c.Request)
}
