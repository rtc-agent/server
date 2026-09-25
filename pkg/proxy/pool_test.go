package proxy

import (
	"testing"
	"time"
)

func TestNewProxyFromConfig(t *testing.T) {
	cfg := ProxyConfig{
		URL:      "socks5://user:pass@proxy.example.com:1080",
		Type:     ProxyTypeSOCKS5,
		Region:   "us",
		Priority: 10,
	}

	p := NewProxyFromConfig(cfg)

	if p.URL != cfg.URL {
		t.Errorf("expected URL %s, got %s", cfg.URL, p.URL)
	}
	if p.Type != cfg.Type {
		t.Errorf("expected Type %v, got %v", cfg.Type, p.Type)
	}
	if p.Region != cfg.Region {
		t.Errorf("expected Region %s, got %s", cfg.Region, p.Region)
	}
	if p.Priority != cfg.Priority {
		t.Errorf("expected Priority %d, got %d", cfg.Priority, p.Priority)
	}
	if p.Auth == nil {
		t.Error("expected Auth to be parsed for SOCKS5")
	} else {
		if p.Auth.User != "user" {
			t.Errorf("expected Auth.User 'user', got '%s'", p.Auth.User)
		}
		if p.Auth.Password != "pass" {
			t.Errorf("expected Auth.Password 'pass', got '%s'", p.Auth.Password)
		}
	}
}

func TestNewProxyFromConfig_HTTP(t *testing.T) {
	cfg := ProxyConfig{
		URL:  "http://proxy.example.com:8080",
		Type: ProxyTypeHTTP,
	}

	p := NewProxyFromConfig(cfg)

	if p.Auth != nil {
		t.Error("expected Auth to be nil for HTTP proxy")
	}
}

func TestNewProxyPool_Empty(t *testing.T) {
	pool := NewProxyPool(nil, "", 0, nil)

	if pool == nil {
		t.Fatal("expected non-nil pool")
	}
	if len(pool.proxies) != 0 {
		t.Errorf("expected 0 proxies, got %d", len(pool.proxies))
	}
}

func TestNewProxyPool_WithProxies(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
		{URL: "http://proxy2.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "https://www.google.com", 30*time.Second, nil)

	if len(pool.proxies) != 2 {
		t.Errorf("expected 2 proxies, got %d", len(pool.proxies))
	}
}

func TestProxyPool_Next(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
		{URL: "http://proxy2.example.com:8080", Type: ProxyTypeHTTP},
		{URL: "http://proxy3.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 30*time.Second, nil)

	// Test round-robin rotation
	p1 := pool.Next()
	p2 := pool.Next()
	p3 := pool.Next()
	p4 := pool.Next() // Should wrap around

	if p1 == nil || p2 == nil || p3 == nil || p4 == nil {
		t.Fatal("expected non-nil proxies")
	}

	// Should cycle through proxies
	if p1.URL == p2.URL || p2.URL == p3.URL {
		t.Error("expected different proxies in sequence")
	}

	// p4 should be same as p1 (wrapped around)
	if p4.URL != p1.URL {
		t.Errorf("expected wrap-around to first proxy, got %s", p4.URL)
	}
}

func TestProxyPool_Next_Empty(t *testing.T) {
	pool := NewProxyPool(nil, "", 30*time.Second, nil)

	p := pool.Next()
	if p != nil {
		t.Error("expected nil proxy from empty pool")
	}
}

func TestProxyPool_NextHealthy_NoHealthData(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
		{URL: "http://proxy2.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 30*time.Second, nil)

	// Without health data, should still return a proxy (round-robin)
	p := pool.NextHealthy()
	if p == nil {
		t.Error("expected non-nil proxy even without health data")
	}
}

func TestProxyPool_HasHealthyProxy_NoHealthData(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 30*time.Second, nil)

	// Without health checks, should return true (assumes healthy)
	if !pool.HasHealthyProxy() {
		t.Error("expected HasHealthyProxy() to return true without health data (assumes healthy)")
	}
}

func TestProxyPool_GetHealth_NoData(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 30*time.Second, nil)

	health := pool.GetHealth("http://proxy1.example.com:8080")
	if health != nil {
		t.Error("expected nil health for untracked proxy")
	}
}

func TestProxyPool_GetHealth_UnknownProxy(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 30*time.Second, nil)

	health := pool.GetHealth("http://unknown.example.com:8080")
	if health != nil {
		t.Error("expected nil health for unknown proxy")
	}
}

func TestProxyPool_StartStop_Empty(t *testing.T) {
	pool := NewProxyPool(nil, "", 30*time.Second, nil)

	// Should not panic
	pool.Start()
	pool.Stop()
}

func TestProxyPool_StartStop_WithProxies(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "https://www.google.com", 100*time.Millisecond, nil)

	// Start should not panic
	pool.Start()

	// Wait for at least one health check
	time.Sleep(150 * time.Millisecond)

	// Stop should not panic
	pool.Stop()
}

func TestProxyPool_Start_MultipleCalls(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 30*time.Second, nil)

	// Multiple Start calls should be safe (sync.Once)
	pool.Start()
	pool.Start()
	pool.Start()

	pool.Stop()
}

func TestProxyPool_DefaultValues(t *testing.T) {
	pool := NewProxyPool(nil, "", 0, nil)

	if pool.healthCheckURL != "https://www.google.com" {
		t.Errorf("expected default healthCheckURL 'https://www.google.com', got '%s'", pool.healthCheckURL)
	}
	if pool.checkInterval != 30*time.Second {
		t.Errorf("expected default checkInterval 30s, got %v", pool.checkInterval)
	}
}

func TestProxyType_Constants(t *testing.T) {
	if ProxyTypeSOCKS5 != "socks5" {
		t.Errorf("expected ProxyTypeSOCKS5 'socks5', got '%s'", ProxyTypeSOCKS5)
	}
	if ProxyTypeHTTP != "http" {
		t.Errorf("expected ProxyTypeHTTP 'http', got '%s'", ProxyTypeHTTP)
	}
	if ProxyTypeHTTPS != "https" {
		t.Errorf("expected ProxyTypeHTTPS 'https', got '%s'", ProxyTypeHTTPS)
	}
	if ProxyTypeDirect != "direct" {
		t.Errorf("expected ProxyTypeDirect 'direct', got '%s'", ProxyTypeDirect)
	}
}

func TestProxyHealth_AtomicOperations(t *testing.T) {
	health := &ProxyHealth{}

	// Test SuccessRate
	health.SuccessRate.Store(5000) // 50.00%
	if health.SuccessRate.Load() != 5000 {
		t.Errorf("expected SuccessRate 5000, got %d", health.SuccessRate.Load())
	}

	// Test AvgLatency
	health.AvgLatency.Store(100000000) // 100ms in nanoseconds
	if health.AvgLatency.Load() != 100000000 {
		t.Errorf("expected AvgLatency 100000000, got %d", health.AvgLatency.Load())
	}

	// Test LastCheck
	health.LastCheck.Store(1234567890)
	if health.LastCheck.Load() != 1234567890 {
		t.Errorf("expected LastCheck 1234567890, got %d", health.LastCheck.Load())
	}
}
