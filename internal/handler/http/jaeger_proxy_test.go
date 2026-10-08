package httphandler

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJaegerProxyHTMLRewriting(t *testing.T) {
	handler := NewJaegerProxyHandler("http://127.0.0.1:26686")
	if handler.proxy == nil {
		t.Skip("Proxy not initialized (Jaeger may not be running)")
	}

	// Create a test request
	req := httptest.NewRequest("GET", "/api/jaeger/", nil)
	w := httptest.NewRecorder()

	// Call the proxy directly
	handler.proxy.ServeHTTP(w, req)

	// Get response
	resp := w.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	t.Logf("Response status: %d", resp.StatusCode)
	t.Logf("Response Content-Type: %s", resp.Header.Get("Content-Type"))
	t.Logf("Full response body:\n%s", string(body))

	// Check if HTML rewriting worked
	bodyStr := string(body)

	// Should NOT contain unmodified paths
	if strings.Contains(bodyStr, `href="./static/`) {
		t.Errorf("HTML still contains unmodified href=\"./static/\" paths")
	}
	if strings.Contains(bodyStr, `src="./static/`) {
		t.Errorf("HTML still contains unmodified src=\"./static/\" paths")
	}
	if strings.Contains(bodyStr, `<base href="/"`) {
		t.Errorf("HTML still contains unmodified <base href=\"/\">")
	}

	// Should contain modified paths
	if !strings.Contains(bodyStr, `href="/api/jaeger/`) && !strings.Contains(bodyStr, `src="/api/jaeger/`) {
		t.Errorf("HTML does not contain expected /api/jaeger/ paths")
	}
}
