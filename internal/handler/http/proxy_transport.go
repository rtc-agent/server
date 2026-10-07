// Package httphandler provides HTTP handler implementations.
package httphandler

import (
	"net/http"
	"time"
)

// sharedProxyTransport is a shared http.Transport for all reverse proxies.
// Reusing a single transport allows connection pooling across all proxy targets,
// reducing resource usage and improving performance (P2 fix: proxy transport reuse).
//
// Each proxy creates its own httputil.ReverseProxy, but they all share this transport
// for underlying HTTP connections. This is safe because http.Transport is designed
// for concurrent use by multiple goroutines.
var sharedProxyTransport = &http.Transport{
	MaxIdleConns:        100,
	IdleConnTimeout:     90 * time.Second,
	MaxIdleConnsPerHost: 100,
}
