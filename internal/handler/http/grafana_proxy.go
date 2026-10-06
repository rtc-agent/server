// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// GrafanaProxyHandler reverse-proxies the Grafana web UI.
// Requests to /api/grafana/* are forwarded to Grafana /api/grafana/*.
// Grafana must be configured with GF_SERVER_SERVE_FROM_SUB_PATH=true
// and GF_SERVER_ROOT_URL including /api/grafana so it generates
// correct asset URLs that match the proxy route.
// If grafanaURL is empty, all requests return 501 Not Implemented.
type GrafanaProxyHandler struct {
	proxy *httputil.ReverseProxy
}

// NewGrafanaProxyHandler creates a new GrafanaProxyHandler.
// If grafanaURL is empty, the handler returns 501 for all requests.
func NewGrafanaProxyHandler(grafanaURL string) *GrafanaProxyHandler {
	if grafanaURL == "" {
		logger.Warn(context.Background(), "admin.grafana_url_not_configured_grafana_proxy_disabled")
		return &GrafanaProxyHandler{}
	}

	target, err := url.Parse(grafanaURL)
	if err != nil {
		logger.Error(context.Background(), "admin.grafana_url_parse_failed_grafana_proxy_disabled",
			zap.String("url", grafanaURL), zap.Error(err))
		return &GrafanaProxyHandler{}
	}

	// Construct ReverseProxy directly (avoid NewSingleHostReverseProxy which sets
	// deprecated Director). Do NOT strip the /api/grafana prefix — Grafana's
	// serve_from_sub_path handles it.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Preserve original Host header so Grafana's origin check matches root_url
			r.Out.Host = r.In.Host
			r.Out.URL.RawPath = ""
		},
		Transport: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 100,
		},
		ModifyResponse: func(resp *http.Response) error {
			// Remove X-Frame-Options and CSP frame-ancestors so the iframe can embed Grafana
			resp.Header.Del("X-Frame-Options")
			resp.Header.Del("Content-Security-Policy")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error(r.Context(), "admin.grafana_proxy_error",
				zap.String("path", r.URL.Path), zap.Error(err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":"error","error":"grafana unavailable"}`))
		},
	}

	logger.Info(context.Background(), "admin.grafana_proxy_enabled",
		zap.String("grafana_url", grafanaURL))

	return &GrafanaProxyHandler{proxy: proxy}
}

// RegisterRoutes registers the grafana proxy routes under the given router group.
// Routes are registered under /grafana/* to match the apiGroup prefix (/api).
func (h *GrafanaProxyHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.Any("/grafana/*path", h.handleProxy)
}

// handleProxy forwards the request to Grafana or returns 501 if not configured.
func (h *GrafanaProxyHandler) handleProxy(c *gin.Context) {
	if h.proxy == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"status": "error",
			"error":  "grafana proxy not configured",
		})
		return
	}
	h.proxy.ServeHTTP(c.Writer, c.Request)
}
