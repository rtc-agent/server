package proxy

import (
	"net/url"
	"sync/atomic"

	"golang.org/x/net/proxy"
)

// ProxyType defines the type of proxy
type ProxyType string

const (
	ProxyTypeSOCKS5 ProxyType = "socks5"
	ProxyTypeHTTP   ProxyType = "http"
	ProxyTypeHTTPS  ProxyType = "https"
	ProxyTypeDirect ProxyType = "direct"
)

// ProxyConfig defines proxy configuration
type ProxyConfig struct {
	URL      string    `json:"url"`      // e.g., socks5://user:pass@host:port
	Type     ProxyType `json:"type"`     // socks5, http, https, direct
	Region   string    `json:"region"`   // e.g., us, cn, jp
	Priority int       `json:"priority"` // Priority (1-10, higher is better)
}

// Proxy represents a proxy server
type Proxy struct {
	URL      string
	Type     ProxyType
	Region   string
	Priority int
	Auth     *proxy.Auth // Parsed from URL for SOCKS5
}

// ProxyHealth tracks proxy health metrics
type ProxyHealth struct {
	SuccessRate atomic.Uint64 // 0-10000 (0-100.00%, two decimal precision)
	AvgLatency  atomic.Int64  // nanoseconds
	LastCheck   atomic.Int64  // unix timestamp
}

// NewProxyFromConfig creates a Proxy from a ProxyConfig, parsing auth for SOCKS5.
func NewProxyFromConfig(cfg ProxyConfig) Proxy {
	p := Proxy{
		URL:      cfg.URL,
		Type:     cfg.Type,
		Region:   cfg.Region,
		Priority: cfg.Priority,
	}

	// Parse authentication from URL for SOCKS5
	if cfg.Type == ProxyTypeSOCKS5 && cfg.URL != "" {
		if u, err := url.Parse(cfg.URL); err == nil && u.User != nil {
			password, _ := u.User.Password()
			p.Auth = &proxy.Auth{
				User:     u.User.Username(),
				Password: password,
			}
		}
	}

	return p
}
