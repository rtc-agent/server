// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rtc-agent/server/pkg/logger"
)

// JaegerProxyHandler reverse-proxies the Jaeger web UI.
// Requests to /api/jaeger/* are forwarded to Jaeger with the /api/jaeger
// prefix stripped, since Jaeger does not support sub-path deployments.
// If jaegerURL is empty, all requests return 501 Not Implemented.
type JaegerProxyHandler struct {
	proxy *httputil.ReverseProxy
}

// NewJaegerProxyHandler creates a new JaegerProxyHandler.
// If jaegerURL is empty, the handler returns 501 for all requests.
func NewJaegerProxyHandler(jaegerURL string) *JaegerProxyHandler {
	if jaegerURL == "" {
		logger.Warn(context.Background(), "admin.jaeger_url_not_configured_jaeger_proxy_disabled")
		return &JaegerProxyHandler{}
	}

	target, err := url.Parse(jaegerURL)
	if err != nil {
		logger.Error(context.Background(), "admin.jaeger_url_parse_failed_jaeger_proxy_disabled",
			zap.String("url", jaegerURL), zap.Error(err))
		return &JaegerProxyHandler{}
	}

	// Construct ReverseProxy directly (avoid NewSingleHostReverseProxy which sets
	// deprecated Director). Strip the /api/jaeger prefix since Jaeger does not
	// support sub-path deployments.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Preserve original Host header
			r.Out.Host = r.In.Host
			// Strip /api/jaeger prefix
			r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, "/api/jaeger")
			if r.Out.URL.Path == "" {
				r.Out.URL.Path = "/"
			}
			r.Out.URL.RawPath = ""
			// Remove Accept-Encoding to prevent Jaeger from returning compressed response
			// (we need to modify the HTML, which doesn't work on compressed data)
			r.Out.Header.Del("Accept-Encoding")
		},
		Transport: sharedProxyTransport,
		ModifyResponse: func(resp *http.Response) error {
			return modifyResponseForProxy(resp, "/api/jaeger")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error(r.Context(), "admin.jaeger_proxy_error",
				zap.String("path", r.URL.Path), zap.Error(err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":"error","error":"jaeger unavailable"}`))
		},
	}

	logger.Info(context.Background(), "admin.jaeger_proxy_enabled",
		zap.String("jaeger_url", jaegerURL))

	return &JaegerProxyHandler{proxy: proxy}
}

// RegisterRoutes registers the jaeger proxy routes under the given router group.
// Routes are registered under /jaeger/* to match the apiGroup prefix (/api).
func (h *JaegerProxyHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.Any("/jaeger/*path", h.handleProxy)
}

// handleProxy forwards the request to Jaeger or returns 501 if not configured.
func (h *JaegerProxyHandler) handleProxy(c *gin.Context) {
	if h.proxy == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"status": "error",
			"error":  "jaeger proxy not configured",
		})
		return
	}
	h.proxy.ServeHTTP(c.Writer, c.Request)
}

// rewriteHTMLForProxy rewrites HTML responses to fix asset paths for reverse proxy.
// It replaces absolute and relative paths with proxy-prefixed paths and updates
// the <base href="/"> tag. Returns the modified body and updates Content-Length.
func rewriteHTMLForProxy(resp *http.Response, proxyPrefix string) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}

	// Rewrite both absolute and relative paths
	body = bytes.ReplaceAll(body, []byte(`href="/`), []byte(`href="`+proxyPrefix+`/`))
	body = bytes.ReplaceAll(body, []byte(`src="/`), []byte(`src="`+proxyPrefix+`/`))
	body = bytes.ReplaceAll(body, []byte(`action="/`), []byte(`action="`+proxyPrefix+`/`))
	body = bytes.ReplaceAll(body, []byte(`href="./`), []byte(`href="`+proxyPrefix+`/`))
	body = bytes.ReplaceAll(body, []byte(`src="./`), []byte(`src="`+proxyPrefix+`/`))
	body = bytes.ReplaceAll(body, []byte(`action="./`), []byte(`action="`+proxyPrefix+`/`))

	// Replace <base href="/"> with proxy-aware base URL (do this AFTER other replacements)
	body = bytes.ReplaceAll(body, []byte(`<base href="/"`), []byte(`<base href="`+proxyPrefix+`/"`))

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// modifyResponseForProxy is a shared ModifyResponse function for reverse proxies
// that need to strip framing headers and rewrite HTML paths.
func modifyResponseForProxy(resp *http.Response, proxyPrefix string) error {
	// Remove X-Frame-Options and CSP frame-ancestors so the iframe can embed the UI
	resp.Header.Del("X-Frame-Options")
	resp.Header.Del("Content-Security-Policy")

	// Rewrite HTML to fix paths for reverse proxy
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		if err := rewriteHTMLForProxy(resp, proxyPrefix); err != nil {
			return err
		}
	}

	return nil
}
