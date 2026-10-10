package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rtc-agent/server/internal/infra/config"
)

func TestCSP_Disabled(t *testing.T) {
	cfg := &config.CSPConfig{
		Enabled: false,
	}

	handler := CSP(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	// Should not set CSP header when disabled
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("expected no CSP header when disabled, got %q", csp)
	}
	if csp := rec.Header().Get("Content-Security-Policy-Report-Only"); csp != "" {
		t.Errorf("expected no CSP-Report-Only header when disabled, got %q", csp)
	}
}

func TestCSP_Enabled_EnforceMode(t *testing.T) {
	cfg := &config.CSPConfig{
		Enabled:    true,
		ReportOnly: false,
		DefaultSrc: []string{"'self'"},
		ScriptSrc:  []string{"'self'", "'unsafe-inline'"},
	}

	handler := CSP(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	// Should set CSP header in enforce mode
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("expected CSP header to be set")
	}

	// Verify report-only header is NOT set
	if cspReport := rec.Header().Get("Content-Security-Policy-Report-Only"); cspReport != "" {
		t.Errorf("expected no CSP-Report-Only header in enforce mode, got %q", cspReport)
	}

	// Verify directives are present
	expected := "default-src 'self'; script-src 'self' 'unsafe-inline'"
	if csp != expected {
		t.Errorf("expected CSP %q, got %q", expected, csp)
	}
}

func TestCSP_Enabled_ReportOnlyMode(t *testing.T) {
	cfg := &config.CSPConfig{
		Enabled:    true,
		ReportOnly: true,
		DefaultSrc: []string{"'self'"},
	}

	handler := CSP(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	// Should set CSP-Report-Only header in report-only mode
	csp := rec.Header().Get("Content-Security-Policy-Report-Only")
	if csp == "" {
		t.Fatal("expected CSP-Report-Only header to be set")
	}

	// Verify enforce header is NOT set
	if cspEnforce := rec.Header().Get("Content-Security-Policy"); cspEnforce != "" {
		t.Errorf("expected no CSP header in report-only mode, got %q", cspEnforce)
	}
}

func TestCSP_WithReportURI(t *testing.T) {
	cfg := &config.CSPConfig{
		Enabled:    true,
		ReportOnly: false,
		DefaultSrc: []string{"'self'"},
		ReportURI:  "/api/csp-report",
	}

	handler := CSP(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("expected CSP header to be set")
	}

	// Verify report-uri is included
	expected := "default-src 'self'; report-uri /api/csp-report"
	if csp != expected {
		t.Errorf("expected CSP %q, got %q", expected, csp)
	}
}

func TestCSP_AllDirectives(t *testing.T) {
	cfg := &config.CSPConfig{
		Enabled:        true,
		ReportOnly:     false,
		DefaultSrc:     []string{"'self'"},
		ScriptSrc:      []string{"'self'", "'unsafe-inline'"},
		StyleSrc:       []string{"'self'", "'unsafe-inline'"},
		ImgSrc:         []string{"'self'", "data:", "blob:"},
		FontSrc:        []string{"'self'", "data:"},
		ConnectSrc:     []string{"'self'", "ws:", "wss:"},
		FrameSrc:       []string{"'none'"},
		FrameAncestors: []string{"'none'"},
		BaseURI:        []string{"'self'"},
		FormAction:     []string{"'self'"},
		ReportURI:      "/api/csp-report",
	}

	handler := CSP(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("expected CSP header to be set")
	}

	// Verify all directives are present
	expectedDirectives := []string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self' ws: wss:",
		"frame-src 'none'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"report-uri /api/csp-report",
	}

	for _, directive := range expectedDirectives {
		if !contains(csp, directive) {
			t.Errorf("expected CSP to contain %q, got %q", directive, csp)
		}
	}
}

func TestCSP_EmptyDirectives(t *testing.T) {
	cfg := &config.CSPConfig{
		Enabled:    true,
		ReportOnly: false,
		// All directives empty
	}

	handler := CSP(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	// Empty directives should result in empty header value
	if csp != "" {
		t.Errorf("expected empty CSP header when no directives configured, got %q", csp)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
