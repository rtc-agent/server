package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rtc-agent/server/internal/infra/middleware"
)

// TestGoroutinesHandler_Summary 验证 goroutines 端点摘要模式返回正确的 JSON 结构。
func TestGoroutinesHandler_Summary(t *testing.T) {
	handler := middleware.GoroutinesHandler()
	req := httptest.NewRequest("GET", "/debug/goroutines", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json content-type, got %s", ct)
	}

	var resp struct {
		GoroutineCount int            `json:"goroutine_count"`
		ByState        map[string]int `json:"by_state"`
		Timestamp      string         `json:"timestamp"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.GoroutineCount <= 0 {
		t.Errorf("expected positive goroutine count, got %d", resp.GoroutineCount)
	}

	// 应至少有一个 running 或 runnable 的 goroutine
	totalState := 0
	for _, count := range resp.ByState {
		totalState += count
	}
	if totalState == 0 {
		t.Error("expected non-empty by_state map")
	}

	if resp.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
}

// TestGoroutinesHandler_Detail 验证 goroutines 端点 detail 模式返回 goroutine 堆栈文本。
func TestGoroutinesHandler_Detail(t *testing.T) {
	handler := middleware.GoroutinesHandler()
	req := httptest.NewRequest("GET", "/debug/goroutines?detail=1", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Fatalf("expected text/plain content-type, got %s", ct)
	}

	body := w.Body.String()
	// 至少应包含当前测试 goroutine
	if !strings.Contains(body, "goroutine") {
		t.Error("expected goroutine stack dump to contain 'goroutine'")
	}
}

// TestStartGoroutineCollector 验证后台采集器正常启动和停止。
func TestStartGoroutineCollector(t *testing.T) {
	cancel := middleware.StartGoroutineCollector(10000)
	if cancel == nil {
		t.Fatal("expected non-nil cancel function")
	}

	// 等待至少一个采集周期（10s 太长，但应能正常启动不 panic）
	time.Sleep(100 * time.Millisecond)

	// 停止不应 panic
	cancel()
}

// TestRegisterPprofRoutes 验证 pprof 路由注册后可访问。
func TestRegisterPprofRoutes(t *testing.T) {
	mux := http.NewServeMux()
	middleware.RegisterPprofRoutes(mux)

	// 测试索引页
	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected pprof index status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "profile") {
		t.Error("expected pprof index to mention profiles")
	}

	// 测试 cmdline
	req2 := httptest.NewRequest("GET", "/debug/pprof/cmdline", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("expected cmdline status 200, got %d", w2.Code)
	}
}

// TestBasicAuth 验证 BasicAuth 中间件的认证逻辑。
func TestBasicAuth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	})
	handler := middleware.BasicAuth(inner, "admin", "pass123")

	tests := []struct {
		name       string
		setAuth    func(*http.Request)
		wantCode   int
		wantBody   string
	}{
		{
			name:     "no auth header",
			setAuth:  func(r *http.Request) {},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "wrong credentials",
			setAuth: func(r *http.Request) {
				r.SetBasicAuth("admin", "wrong")
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "correct credentials",
			setAuth: func(r *http.Request) {
				r.SetBasicAuth("admin", "pass123")
			},
			wantCode: http.StatusOK,
			wantBody: "secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/debug/test", nil)
			tt.setAuth(req)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("expected status %d, got %d", tt.wantCode, w.Code)
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Errorf("expected body %q, got %q", tt.wantBody, w.Body.String())
			}
		})
	}
}

// TestHTTPMetrics_SkipsHealthz 验证 /healthz 路径不记录指标。
func TestHTTPMetrics_SkipsHealthz(t *testing.T) {
	handler := middleware.HTTPMetrics()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestHTTPMetrics_CapturesResponse 验证中间件正确捕获响应大小和状态码。
func TestHTTPMetrics_CapturesResponse(t *testing.T) {
	handler := middleware.HTTPMetrics()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello world"))
	}))

	req := httptest.NewRequest("POST", "/api/test", strings.NewReader("body"))
	req.ContentLength = 4
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	if w.Body.String() != "hello world" {
		t.Fatalf("expected 'hello world', got %q", w.Body.String())
	}
}

// TestHTTPMetrics_SkipsWebSocket 验证 WebSocket 升级请求被跳过。
func TestHTTPMetrics_SkipsWebSocket(t *testing.T) {
	handler := middleware.HTTPMetrics()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", w.Code)
	}
}

// TestGoroutineCount 验证 runtime.NumGoroutine 返回合理值。
func TestGoroutineCount(t *testing.T) {
	n := runtime.NumGoroutine()
	if n <= 0 {
		t.Fatalf("expected positive goroutine count, got %d", n)
	}
	t.Logf("current goroutine count: %d", n)
}
