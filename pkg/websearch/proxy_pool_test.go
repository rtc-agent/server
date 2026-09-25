package websearch

import (
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func TestProxyPool_EmptyPool(t *testing.T) {
	pool := NewProxyPool(nil, "", 0)

	// Next should return nil for empty pool
	if p := pool.Next(); p != nil {
		t.Error("expected nil proxy from empty pool")
	}

	// NextHealthy should return nil for empty pool
	if p := pool.NextHealthy(); p != nil {
		t.Error("expected nil proxy from empty pool")
	}

	// HasHealthyProxy should return true for empty pool (direct connection)
	if !pool.HasHealthyProxy() {
		t.Error("expected HasHealthyProxy=true for empty pool")
	}
}

func TestProxyPool_RoundRobin(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP, Region: "us"},
		{URL: "http://proxy2.example.com:8080", Type: ProxyTypeHTTP, Region: "jp"},
		{URL: "http://proxy3.example.com:8080", Type: ProxyTypeHTTP, Region: "sg"},
	}

	pool := NewProxyPool(configs, "", 0)

	// Test round-robin selection
	selected := make(map[string]int)
	for i := 0; i < 9; i++ {
		p := pool.Next()
		if p == nil {
			t.Fatal("unexpected nil proxy")
		}
		selected[p.URL]++
	}

	// Each proxy should be selected 3 times
	for _, count := range selected {
		if count != 3 {
			t.Errorf("expected 3 selections per proxy, got %v", selected)
			break
		}
	}
}

func TestProxyPool_HealthTracking(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 0)

	// Initially no health data
	health := pool.GetHealth("http://proxy1.example.com:8080")
	if health != nil {
		t.Error("expected nil health initially")
	}

	// Simulate health check by loading health data
	healthData, _ := pool.healthMap.LoadOrStore("http://proxy1.example.com:8080", &ProxyHealth{})
	h := healthData.(*ProxyHealth)

	// Update success rate
	h.SuccessRate.Store(9500) // 95%
	h.AvgLatency.Store(int64(100 * time.Millisecond))
	h.LastCheck.Store(time.Now().Unix())

	// Verify health data
	health = pool.GetHealth("http://proxy1.example.com:8080")
	if health == nil {
		t.Fatal("expected health data after update")
	}

	if health.SuccessRate.Load() != 9500 {
		t.Errorf("expected success rate 9500, got %d", health.SuccessRate.Load())
	}

	if health.AvgLatency.Load() != int64(100*time.Millisecond) {
		t.Errorf("expected latency 100ms, got %d", health.AvgLatency.Load())
	}
}

func TestProxyPool_NextHealthy(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://healthy.example.com:8080", Type: ProxyTypeHTTP},
		{URL: "http://unhealthy.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "", 0)

	// Mark first proxy as healthy
	healthData, _ := pool.healthMap.LoadOrStore("http://healthy.example.com:8080", &ProxyHealth{})
	healthData.(*ProxyHealth).SuccessRate.Store(9500) // 95%

	// Mark second proxy as unhealthy
	healthData2, _ := pool.healthMap.LoadOrStore("http://unhealthy.example.com:8080", &ProxyHealth{})
	healthData2.(*ProxyHealth).SuccessRate.Store(5000) // 50%

	// NextHealthy should prefer healthy proxy
	selected := make(map[string]int)
	for i := 0; i < 10; i++ {
		p := pool.NextHealthy()
		if p == nil {
			t.Fatal("unexpected nil proxy")
		}
		selected[p.URL]++
	}

	// Healthy proxy should be selected most of the time
	if selected["http://healthy.example.com:8080"] < 8 {
		t.Errorf("expected healthy proxy to be selected at least 8 times, got %v", selected)
	}
}

func TestProxyPool_StartStop(t *testing.T) {
	configs := []ProxyConfig{
		{URL: "http://proxy1.example.com:8080", Type: ProxyTypeHTTP},
	}

	pool := NewProxyPool(configs, "https://www.google.com", 100*time.Millisecond)

	pool.Start()

	// Wait for at least one health check
	time.Sleep(150 * time.Millisecond)

	// Stop should complete without hanging
	done := make(chan struct{})
	go func() {
		pool.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Error("Stop() did not complete within timeout")
	}
}

func TestCreateHTTPClient_Direct(t *testing.T) {
	client, err := createHTTPClient(nil)
	if err != nil {
		t.Fatalf("failed to create direct client: %v", err)
	}
	if client == nil {
		t.Error("expected non-nil client")
	}
}

func TestCreateHTTPClient_HTTPProxy(t *testing.T) {
	p := &Proxy{
		URL:  "http://proxy.example.com:8080",
		Type: ProxyTypeHTTP,
	}

	client, err := createHTTPClient(p)
	if err != nil {
		t.Fatalf("failed to create HTTP proxy client: %v", err)
	}
	if client == nil {
		t.Error("expected non-nil client")
	}
}

func TestCreateHTTPClient_SOCKS5Proxy(t *testing.T) {
	p := &Proxy{
		URL:  "socks5://user:pass@proxy.example.com:1080",
		Type: ProxyTypeSOCKS5,
		Auth: &proxy.Auth{
			User:     "user",
			Password: "pass",
		},
	}

	client, err := createHTTPClient(p)
	if err != nil {
		t.Fatalf("failed to create SOCKS5 proxy client: %v", err)
	}
	if client == nil {
		t.Error("expected non-nil client")
	}
}
