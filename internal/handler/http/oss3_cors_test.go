package httphandler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rtc-agent/server/internal/infra/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOSS3CORSMiddleware_WildcardOrigin(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := NewOSS3CORSMiddleware([]string{"*"})(inner)

	t.Run("OPTIONS preflight returns correct headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PUT")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "GET")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "x-amz-content-sha256")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "x-amz-date")
		assert.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "ETag")
	})

	t.Run("GET request passes through with CORS headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

func TestNewOSS3CORSMiddleware_SpecificOrigins(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	allowed := []string{"https://app.example.com", "https://admin.example.com"}
	handler := NewOSS3CORSMiddleware(allowed)(inner)

	t.Run("allowed origin gets CORS headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "Origin", rec.Header().Get("Vary"))
	})

	t.Run("disallowed origin gets 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestNewOSS3CORSMiddleware_EmptyOrigins(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := NewOSS3CORSMiddleware(nil)(inner)

	t.Run("empty origins denies all CORS requests", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "http://any-origin.com")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "", rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

func TestContainsOSS3Wildcard(t *testing.T) {
	tests := []struct {
		name     string
		origins  []string
		expected bool
	}{
		{"wildcard first", []string{"*", "https://example.com"}, true},
		{"wildcard second", []string{"https://example.com", "*"}, true},
		{"wildcard middle", []string{"https://a.com", "*", "https://b.com"}, true},
		{"no wildcard", []string{"https://a.com", "https://b.com"}, false},
		{"empty", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsOSS3Wildcard(tt.origins)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestNewOSS3CORSMiddleware_WildcardNotFirst(t *testing.T) {
	// Regression: previously only origins[0] was checked for "*",
	// so a whitelist like ["https://app.com", "*"] would reject all origins.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := NewOSS3CORSMiddleware([]string{"https://app.example.com", "*"})(inner)

	req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
	req.Header.Set("Origin", "http://any-origin.com")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// With "*" anywhere in the list, all origins should be allowed.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSNonBrowserRequest(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// With specific origins configured (not wildcard)
	handler := NewOSS3CORSMiddleware([]string{"https://app.example.com"})(inner)

	t.Run("empty origin bypasses CORS and reaches handler", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		// Do NOT set Origin header — simulates CLI/SDK/curl request
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "OK", rec.Body.String())
		// No CORS headers should be set for non-browser requests
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("empty origin with empty origins config still passes through", func(t *testing.T) {
		// Even when CORS is configured to deny all, non-browser requests should pass
		denyAllHandler := NewOSS3CORSMiddleware(nil)(inner)
		req := httptest.NewRequest(http.MethodPost, "/rtc-agent/upload", nil)
		rec := httptest.NewRecorder()

		denyAllHandler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("empty origin OPTIONS still bypasses", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/rtc-agent/test", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		// Should reach the inner handler, not short-circuit with preflight response
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("origin present still performs CORS check", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("origin present but disallowed still blocked", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rtc-agent/test", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestIsOSS3OriginAllowed(t *testing.T) {
	tests := []struct {
		name     string
		origin   string
		allowed  []string
		expected bool
	}{
		{"wildcard allows any", "http://example.com", []string{"*"}, true},
		{"exact match", "https://app.example.com", []string{"https://app.example.com"}, true},
		{"case insensitive", "HTTPS://APP.EXAMPLE.COM", []string{"https://app.example.com"}, true},
		{"no match", "https://evil.com", []string{"https://app.example.com"}, false},
		{"empty allowed", "http://example.com", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := middleware.IsOriginAllowed(tt.origin, tt.allowed)
			require.Equal(t, tt.expected, result)
		})
	}
}
