package svc

import (
	stdcontext "context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rtc-agent/server/pkg/logger"
)

func init() {
	// Initialize logger for tests
	logger.Init("debug", "")
}

func TestAuthFailureLimiter_Basic(t *testing.T) {
	// Create a limiter: 10 failures per minute, burst 10, block for 5 minutes
	limiter := NewAuthFailureLimiter(10.0/60.0, 10, 30*time.Minute, 5*time.Minute)

	ctx := stdcontext.Background()
	ip := "192.168.1.1"

	// First 10 failures should not block
	for i := 0; i < 10; i++ {
		blocked := limiter.RecordFailure(ctx, ip)
		if blocked {
			t.Errorf("IP should not be blocked after %d failures", i+1)
		}
	}

	// 11th failure should trigger blocking
	blocked := limiter.RecordFailure(ctx, ip)
	if !blocked {
		t.Error("IP should be blocked after 11 failures")
	}

	// Subsequent checks should also be blocked
	blocked = limiter.IsBlocked(ctx, ip)
	if !blocked {
		t.Error("IP should remain blocked")
	}
}

func TestAuthFailureLimiter_DifferentIPs(t *testing.T) {
	limiter := NewAuthFailureLimiter(10.0/60.0, 10, 30*time.Minute, 5*time.Minute)

	ctx := stdcontext.Background()
	ip1 := "192.168.1.1"
	ip2 := "192.168.1.2"

	// Block IP1
	for i := 0; i < 11; i++ {
		limiter.RecordFailure(ctx, ip1)
	}

	// IP1 should be blocked
	if !limiter.IsBlocked(ctx, ip1) {
		t.Error("IP1 should be blocked")
	}

	// IP2 should not be blocked
	if limiter.IsBlocked(ctx, ip2) {
		t.Error("IP2 should not be blocked")
	}
}

func TestAuthFailureLimiter_BlockExpiry(t *testing.T) {
	// Create a limiter with short block duration for testing
	limiter := NewAuthFailureLimiter(10.0/60.0, 10, 30*time.Minute, 100*time.Millisecond)

	ctx := stdcontext.Background()
	ip := "192.168.1.1"

	// Trigger blocking
	for i := 0; i < 11; i++ {
		limiter.RecordFailure(ctx, ip)
	}

	// Should be blocked
	if !limiter.IsBlocked(ctx, ip) {
		t.Error("IP should be blocked initially")
	}

	// Wait for block to expire
	time.Sleep(150 * time.Millisecond)

	// Should no longer be blocked
	if limiter.IsBlocked(ctx, ip) {
		t.Error("IP should not be blocked after expiry")
	}
}

func TestAuthFailureLimiter_Cleanup(t *testing.T) {
	limiter := NewAuthFailureLimiter(10.0/60.0, 10, 100*time.Millisecond, 5*time.Minute)

	ctx := stdcontext.Background()
	ip := "192.168.1.1"

	// Create an entry
	limiter.RecordFailure(ctx, ip)

	// Verify entry exists
	if _, ok := limiter.limiters.Load(ip); !ok {
		t.Error("Entry should exist after recording failure")
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Run cleanup
	limiter.Cleanup()

	// Entry should be removed
	if _, ok := limiter.limiters.Load(ip); ok {
		t.Error("Entry should be removed after cleanup")
	}
}

func TestAuthFailureLimiter_ExtractClientIP(t *testing.T) {
	limiter := NewAuthFailureLimiter(10.0/60.0, 10, 30*time.Minute, 5*time.Minute)
	// Configure trusted proxies for testing
	err := limiter.SetTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("Failed to set trusted proxies: %v", err)
	}

	tests := []struct {
		name       string
		setupReq   func() *http.Request
		wantIP     string
		wantSubstr bool // if true, check if result contains wantIP
	}{
		{
			name: "remote_addr_only",
			setupReq: func() *http.Request {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = "192.168.1.100:12345"
				return req
			},
			wantIP: "192.168.1.100",
		},
		{
			name: "x_forwarded_for_single",
			setupReq: func() *http.Request {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = "10.0.0.1:12345" // trusted proxy
				req.Header.Set("X-Forwarded-For", "203.0.113.50")
				return req
			},
			wantIP: "203.0.113.50",
		},
		{
			name: "x_forwarded_for_multiple",
			setupReq: func() *http.Request {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = "10.0.0.1:12345" // trusted proxy
				req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.41.3.18")
				return req
			},
			wantIP: "203.0.113.50",
		},
		{
			name: "x_real_ip",
			setupReq: func() *http.Request {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = "10.0.0.1:12345" // trusted proxy
				req.Header.Set("X-Real-IP", "203.0.113.50")
				return req
			},
			wantIP: "203.0.113.50",
		},
		{
			name: "no_context",
			setupReq: func() *http.Request {
				return nil
			},
			wantIP:     "unknown",
			wantSubstr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq()
			var ctx stdcontext.Context
			if req != nil {
				ctx = stdcontext.WithValue(stdcontext.Background(), http.ServerContextKey, req)
			} else {
				ctx = stdcontext.Background()
			}

			gotIP := limiter.extractClientIP(ctx)

			if tt.wantSubstr {
				if gotIP != tt.wantIP {
					t.Errorf("extractClientIP() = %v, want %v", gotIP, tt.wantIP)
				}
			} else {
				if gotIP != tt.wantIP {
					t.Errorf("extractClientIP() = %v, want %v", gotIP, tt.wantIP)
				}
			}
		})
	}
}

func TestAuthFailureLimiter_ConcurrentAccess(t *testing.T) {
	limiter := NewAuthFailureLimiter(10.0/60.0, 10, 30*time.Minute, 5*time.Minute)

	ctx := stdcontext.Background()
	ip := "192.168.1.1"

	// Run concurrent failures
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			limiter.RecordFailure(ctx, ip)
			limiter.IsBlocked(ctx, ip)
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 100; i++ {
		<-done
	}

	// Should not panic and should complete successfully
	// The test passing without panic is the success criteria
}
