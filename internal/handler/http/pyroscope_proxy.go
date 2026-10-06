// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// PyroscopeProxyHandler reverse-proxies the Pyroscope web UI.
// Requests to /api/pyroscope/* are forwarded to Pyroscope with the
// /api/pyroscope prefix stripped, since Pyroscope does not support
// sub-path deployments.
// If pyroscopeURL is empty, all requests return 501 Not Implemented.
type PyroscopeProxyHandler struct {
	proxy *httputil.ReverseProxy
}

// NewPyroscopeProxyHandler creates a new PyroscopeProxyHandler.
// If pyroscopeURL is empty, the handler returns 501 for all requests.
func NewPyroscopeProxyHandler(pyroscopeURL string) *PyroscopeProxyHandler {
	if pyroscopeURL == "" {
		logger.Warn(context.Background(), "admin.pyroscope_url_not_configured_pyroscope_proxy_disabled")
		return &PyroscopeProxyHandler{}
	}

	target, err := url.Parse(pyroscopeURL)
	if err != nil {
		logger.Error(context.Background(), "admin.pyroscope_url_parse_failed_pyroscope_proxy_disabled",
			zap.String("url", pyroscopeURL), zap.Error(err))
		return &PyroscopeProxyHandler{}
	}

	// Construct ReverseProxy directly (avoid NewSingleHostReverseProxy which sets
	// deprecated Director). Strip the /api/pyroscope prefix since Pyroscope does not
	// support sub-path deployments.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Preserve original Host header
			r.Out.Host = r.In.Host
			// Strip /api/pyroscope prefix
			r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, "/api/pyroscope")
			if r.Out.URL.Path == "" {
				r.Out.URL.Path = "/"
			}
			r.Out.URL.RawPath = ""
		},
		Transport: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 100,
		},
		ModifyResponse: func(resp *http.Response) error {
			return modifyResponseForProxy(resp, "/api/pyroscope")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error(r.Context(), "admin.pyroscope_proxy_error",
				zap.String("path", r.URL.Path), zap.Error(err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":"error","error":"pyroscope unavailable"}`))
		},
	}

	logger.Info(context.Background(), "admin.pyroscope_proxy_enabled",
		zap.String("pyroscope_url", pyroscopeURL))

	return &PyroscopeProxyHandler{proxy: proxy}
}

// RegisterRoutes registers the pyroscope proxy routes under the given router group.
// Routes are registered under /pyroscope/* to match the apiGroup prefix (/api).
func (h *PyroscopeProxyHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.Any("/pyroscope/*path", h.handleProxy)
}

// handleProxy forwards the request to Pyroscope or returns 501 if not configured.
func (h *PyroscopeProxyHandler) handleProxy(c *gin.Context) {
	if h.proxy == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"status": "error",
			"error":  "pyroscope proxy not configured",
		})
		return
	}
	h.proxy.ServeHTTP(c.Writer, c.Request)
}
