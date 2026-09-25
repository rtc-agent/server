package webfetch

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SecurityChecker — URL validation
// ---------------------------------------------------------------------------

func TestSecurityChecker_CheckURL_ValidHTTP(t *testing.T) {
	s := NewSecurityChecker(DefaultWebFetchConfig())
	// Valid public URLs should pass format checks.
	// Note: CheckURL also does DNS resolution, so we test with a known public host.
	err := s.CheckURL(context.Background(), "https://example.com/page")
	// May fail on DNS in sandboxed CI; only assert no panic and error type.
	if err != nil {
		assert.NotErrorIs(t, err, ErrInvalidURL)
	}
}

func TestSecurityChecker_CheckURL_BadScheme(t *testing.T) {
	s := NewSecurityChecker(DefaultWebFetchConfig())
	err := s.CheckURL(context.Background(), "ftp://example.com/file")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scheme")
}

func TestSecurityChecker_CheckURL_FileScheme(t *testing.T) {
	s := NewSecurityChecker(DefaultWebFetchConfig())
	err := s.CheckURL(context.Background(), "file:///etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scheme")
}

func TestSecurityChecker_CheckURL_Credentials(t *testing.T) {
	s := NewSecurityChecker(DefaultWebFetchConfig())
	err := s.CheckURL(context.Background(), "http://user:pass@example.com/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential")
}

func TestSecurityChecker_CheckURL_EmptyHost(t *testing.T) {
	s := NewSecurityChecker(DefaultWebFetchConfig())
	err := s.CheckURL(context.Background(), "http:///path-only")
	require.Error(t, err)
}

func TestSecurityChecker_CheckURL_BlockedDomain(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	cfg.BlockedDomains = []string{"evil.com"}
	s := NewSecurityChecker(cfg)
	err := s.CheckURL(context.Background(), "http://evil.com/malware")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDomainBlocked)
}

func TestSecurityChecker_CheckURL_BlockedSubdomain(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	cfg.BlockedDomains = []string{"evil.com"}
	s := NewSecurityChecker(cfg)
	err := s.CheckURL(context.Background(), "http://sub.evil.com/path")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDomainBlocked)
}

// ---------------------------------------------------------------------------
// SSRF — private IP blocking
// ---------------------------------------------------------------------------

func TestIsPrivateIP_PrivateRanges(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"127.0.0.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"100.64.0.1", true},      // CGN
		{"0.0.0.0", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false}, // example.com
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip)
			assert.Equal(t, tt.private, isPrivateIP(ip))
		})
	}
}

func TestIsPrivateIP_IPv6(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"::1", true},
		{"fc00::1", true},
		{"fe80::1", true},
		{"2001:4860:4860::8888", false}, // Google DNS
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip)
			assert.Equal(t, tt.private, isPrivateIP(ip))
		})
	}
}

// ---------------------------------------------------------------------------
// IsPreApproved
// ---------------------------------------------------------------------------

func TestIsPreApproved_ExactMatch(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	s := NewSecurityChecker(cfg)
	assert.True(t, s.IsPreApproved("github.com"))
	assert.True(t, s.IsPreApproved("react.dev"))
	assert.False(t, s.IsPreApproved("unknown-site.com"))
}

func TestIsPreApproved_Subdomain(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	s := NewSecurityChecker(cfg)
	assert.True(t, s.IsPreApproved("docs.python.org"))
	assert.True(t, s.IsPreApproved("pkg.go.dev"))
	assert.True(t, s.IsPreApproved("docs.aws.amazon.com"))
}

func TestIsPreApproved_PathEntry_MatchesDomain(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	s := NewSecurityChecker(cfg)
	// "github.com/anthropics" — the domain part "github.com" should still match.
	assert.True(t, s.IsPreApproved("github.com"))
}

func TestIsPreApprovedWithPath_PathMatch(t *testing.T) {
	// Use a domain that only has path-level entries to test path matching.
	cfg := WebFetchConfig{PreApprovedDomains: []string{"github.com/anthropics"}}
	s := NewSecurityChecker(cfg)
	assert.True(t, s.IsPreApprovedWithPath("github.com", "/anthropics/repo"))
	assert.False(t, s.IsPreApprovedWithPath("github.com", "/evil-org/repo"))
}

func TestIsPreApprovedWithPath_SegmentBoundary(t *testing.T) {
	cfg := WebFetchConfig{PreApprovedDomains: []string{"github.com/anthropics"}}
	s := NewSecurityChecker(cfg)
	assert.True(t, s.IsPreApprovedWithPath("github.com", "/anthropics"))
	assert.False(t, s.IsPreApprovedWithPath("github.com", "/anthropics-evil"))
}

// ---------------------------------------------------------------------------
// validateRequest
// ---------------------------------------------------------------------------

func TestValidateRequest_NilRequest(t *testing.T) {
	m := &WebFetchManager{config: DefaultWebFetchConfig()}
	err := m.validateRequest(nil)
	assert.ErrorIs(t, err, ErrInvalidURL)
}

func TestValidateRequest_EmptyURL(t *testing.T) {
	m := &WebFetchManager{config: DefaultWebFetchConfig()}
	err := m.validateRequest(&FetchRequest{URL: ""})
	assert.ErrorIs(t, err, ErrInvalidURL)
	assert.Contains(t, err.Error(), "URL is empty")
}

func TestValidateRequest_URLTooLong(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	cfg.MaxURLLength = 50
	m := &WebFetchManager{config: cfg}
	longURL := "https://example.com/" + string(make([]byte, 100))
	err := m.validateRequest(&FetchRequest{URL: longURL})
	assert.ErrorIs(t, err, ErrInvalidURL)
	assert.Contains(t, err.Error(), "URL exceeds max length")
}

func TestValidateRequest_ValidURL(t *testing.T) {
	m := &WebFetchManager{config: DefaultWebFetchConfig()}
	err := m.validateRequest(&FetchRequest{URL: "https://example.com"})
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// buildCacheKey — collision resistance
// ---------------------------------------------------------------------------

func TestBuildCacheKey_DifferentInputs(t *testing.T) {
	m := &WebFetchManager{config: DefaultWebFetchConfig()}
	k1 := m.buildCacheKey("https://example.com/a", "prompt1")
	k2 := m.buildCacheKey("https://example.com/b", "prompt1")
	k3 := m.buildCacheKey("https://example.com/a", "prompt2")
	assert.NotEqual(t, k1, k2)
	assert.NotEqual(t, k1, k3)
}

func TestBuildCacheKey_PipeCharNoCollision(t *testing.T) {
	m := &WebFetchManager{config: DefaultWebFetchConfig()}
	// URL containing "|" should not collide with a URL+prompt combo
	k1 := m.buildCacheKey("https://example.com/a|b", "")
	k2 := m.buildCacheKey("https://example.com/a", "b")
	assert.NotEqual(t, k1, k2)
}

// ---------------------------------------------------------------------------
// truncateContent — rune-safe truncation
// ---------------------------------------------------------------------------

func TestTruncateContent_ShortContent(t *testing.T) {
	result := truncateContent("hello world", 100)
	assert.Equal(t, "hello world", result)
}

func TestTruncateContent_LongContent(t *testing.T) {
	content := strings.Repeat("x", 200)
	result := truncateContent(content, 50)
	assert.Contains(t, result, "[... truncated ...]")
	assert.LessOrEqual(t, len([]rune(result)), 60) // allow small margin for suffix
}

func TestTruncateContent_MultiByteRunes(t *testing.T) {
	// Chinese characters are 3 bytes each in UTF-8
	content := "你好世界测试内容" + strings.Repeat("中", 100)
	result := truncateContent(content, 10)
	assert.Contains(t, result, "[... truncated ...]")
}

// ---------------------------------------------------------------------------
// FormatFetchResponse
// ---------------------------------------------------------------------------

func TestFormatFetchResponse_Plain(t *testing.T) {
	resp := &FetchResponse{Result: "hello content"}
	assert.Equal(t, "hello content", FormatFetchResponse(resp))
}

func TestFormatFetchResponse_Cached(t *testing.T) {
	resp := &FetchResponse{URL: "https://example.com", Result: "data", Cached: true}
	out := FormatFetchResponse(resp)
	assert.Contains(t, out, "[cached]")
	assert.Contains(t, out, "data")
}

func TestFormatFetchResponse_LLMMined(t *testing.T) {
	resp := &FetchResponse{URL: "https://example.com", Result: "extracted", LLMMined: true}
	out := FormatFetchResponse(resp)
	assert.Contains(t, out, "[llm-extracted]")
}

// ---------------------------------------------------------------------------
// formatRedirectMessage
// ---------------------------------------------------------------------------

func TestFormatRedirectMessage(t *testing.T) {
	msg := formatRedirectMessage("https://old.com/page", "https://new.com/page", 301)
	assert.Contains(t, msg, "REDIRECT DETECTED")
	assert.Contains(t, msg, "https://old.com/page")
	assert.Contains(t, msg, "https://new.com/page")
	assert.Contains(t, msg, "301")
	assert.Contains(t, msg, "Moved Permanently")
}

// ---------------------------------------------------------------------------
// DomainSemaphore
// ---------------------------------------------------------------------------

func TestDomainSemaphore_AcquireRelease(t *testing.T) {
	ds := NewDomainSemaphore(2)
	ctx := context.Background()

	require.NoError(t, ds.Acquire(ctx, "example.com"))
	require.NoError(t, ds.Acquire(ctx, "example.com"))

	// Third acquire should block — test with canceled context.
	ctxCancel, cancel := context.WithCancel(ctx)
	cancel()
	err := ds.Acquire(ctxCancel, "example.com")
	assert.Error(t, err)

	// Release one slot.
	ds.Release("example.com")
	require.NoError(t, ds.Acquire(ctx, "example.com"))
}

func TestDomainSemaphore_Cleanup(t *testing.T) {
	ds := NewDomainSemaphore(5)
	ctx := context.Background()

	_ = ds.Acquire(ctx, "a.com")
	ds.Release("a.com")
	_ = ds.Acquire(ctx, "b.com")
	// b.com still held, a.com is idle.

	ds.cleanup()
	ds.mu.Lock()
	_, aExists := ds.domains["a.com"]
	_, bExists := ds.domains["b.com"]
	ds.mu.Unlock()
	assert.False(t, aExists, "idle domain should be cleaned up")
	assert.True(t, bExists, "active domain should remain")
}

// ---------------------------------------------------------------------------
// classifyError
// ---------------------------------------------------------------------------

func TestClassifyError(t *testing.T) {
	assert.Equal(t, "invalid_url", classifyError(ErrInvalidURL))
	assert.Equal(t, "ssrf_blocked", classifyError(ErrSSRFBlocked))
	assert.Equal(t, "timeout", classifyError(ErrTimeout))
	assert.Equal(t, "unknown", classifyError(net.ErrClosed))
}

func TestIsRetryable(t *testing.T) {
	assert.True(t, isRetryable(ErrTimeout))
	assert.True(t, isRetryable(ErrServerError))
	assert.True(t, isRetryable(ErrConnectionReset))
	assert.False(t, isRetryable(ErrInvalidURL))
	assert.False(t, isRetryable(ErrSSRFBlocked))
	assert.False(t, isRetryable(ErrClientError))
}

// ---------------------------------------------------------------------------
// HTML conversion
// ---------------------------------------------------------------------------

func TestHTMLToMarkdown_Basic(t *testing.T) {
	html := []byte("<h1>Title</h1><p>Hello <strong>world</strong></p>")
	md, err := htmlToMarkdown(html)
	require.NoError(t, err)
	assert.Contains(t, md, "Title")
	assert.Contains(t, md, "world")
}

func TestStripHTML_Basic(t *testing.T) {
	html := []byte("<h1>Title</h1><p>Hello world</p>")
	text := stripHTML(html)
	assert.Contains(t, text, "Title")
	assert.Contains(t, text, "Hello world")
	assert.NotContains(t, text, "<")
}

func TestNormalizeToUTF8_AlreadyUTF8(t *testing.T) {
	body := []byte("hello 世界")
	result := normalizeToUTF8(body, "utf-8", nil)
	assert.Equal(t, body, result)
}

func TestNormalizeToUTF8_EmptyCharset(t *testing.T) {
	body := []byte("hello")
	result := normalizeToUTF8(body, "", nil)
	assert.Equal(t, body, result)
}

func TestDetectCharset_FromHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=gbk")
	result := detectCharset(h)
	assert.Equal(t, "gbk", result)
}

func TestDetectCharset_NoCharset(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/html")
	result := detectCharset(h)
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestWebFetchManager_Lifecycle(t *testing.T) {
	m, err := NewWebFetchManager(DefaultWebFetchConfig(), nil, nil, nil)
	require.NoError(t, err)

	// HealthCheck before Start should fail.
	err = m.HealthCheck(context.Background())
	assert.Error(t, err)

	// Start.
	require.NoError(t, m.Start(context.Background()))

	// Double start should fail.
	err = m.Start(context.Background())
	assert.Error(t, err)

	// HealthCheck after Start should pass.
	err = m.HealthCheck(context.Background())
	assert.NoError(t, err)

	// Stop.
	require.NoError(t, m.Stop(context.Background()))

	// HealthCheck after Stop should fail.
	err = m.HealthCheck(context.Background())
	assert.Error(t, err)
}

func TestWebFetchManager_FetchAfterShutdown(t *testing.T) {
	m, err := NewWebFetchManager(DefaultWebFetchConfig(), nil, nil, nil)
	require.NoError(t, err)
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.Stop(context.Background()))

	_, err = m.Fetch(context.Background(), &FetchRequest{URL: "https://example.com"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "shutting down")
}

// ---------------------------------------------------------------------------
// Concurrency — acquireSlots / releaseSlots
// ---------------------------------------------------------------------------

func TestAcquireSlots_GlobalFailFast(t *testing.T) {
	cfg := DefaultWebFetchConfig()
	cfg.MaxConcurrency = 1
	m, _ := NewWebFetchManager(cfg, nil, nil, nil)

	// Fill the global slot.
	m.globalSem <- struct{}{}

	// Next acquire should fail immediately.
	parsedURL := mustParseURL("https://example.com")
	err := m.acquireSlots(context.Background(), parsedURL)
	assert.ErrorIs(t, err, ErrConcurrencyLimit)
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

// ---------------------------------------------------------------------------
// Integration test — real HTTP fetch
// ---------------------------------------------------------------------------

func TestWebFetchManager_FetchRealURL(t *testing.T) {
	// This test makes a real HTTP request to GitHub.
	// It verifies the full pipeline: validate → SSRF → rate limit → HTTP → process.
	cfg := DefaultWebFetchConfig()
	cfg.FetchTimeout = 30 * time.Second

	// Use nil Redis client — cache will be no-op.
	manager, err := NewWebFetchManager(cfg, nil, nil, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = manager.Start(ctx)
	require.NoError(t, err)
	defer manager.Stop(ctx)

	resp, err := manager.Fetch(ctx, &FetchRequest{
		URL:       "https://github.com/anthropics/anthropic-sdk-go",
		Prompt:    "What is this repository about?",
		SessionID: "test-session",
	})
	require.NoError(t, err, "Fetch should succeed for github.com")
	require.NotNil(t, resp)

	// Verify response fields.
	assert.NotEmpty(t, resp.URL)
	assert.Equal(t, 200, resp.Code)
	assert.NotEmpty(t, resp.CodeText)
	assert.True(t, resp.Bytes > 0, "should have received content")
	assert.NotEmpty(t, resp.Result, "result should contain markdown content")
	assert.False(t, resp.Cached, "first fetch should not be cached")
	assert.Contains(t, resp.Result, "anthropic", "result should mention anthropic")

	t.Logf("Fetch succeeded: code=%d bytes=%d duration=%dms cached=%v",
		resp.Code, resp.Bytes, resp.DurationMs, resp.Cached)
	t.Logf("Result preview (first 500 chars): %s", truncatePreview(resp.Result, 500))
}

func TestWebFetchManager_FetchRealURL_CacheHit(t *testing.T) {
	// Verify that fetching the same URL twice results in a cache hit (using miniredis).
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer redisClient.Close()

	cfg := DefaultWebFetchConfig()
	cfg.FetchTimeout = 30 * time.Second
	cfg.CacheTTL = 5 * time.Minute

	manager, err := NewWebFetchManager(cfg, redisClient, nil, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = manager.Start(ctx)
	require.NoError(t, err)
	defer manager.Stop(ctx)

	// First fetch.
	resp1, err := manager.Fetch(ctx, &FetchRequest{
		URL:       "https://github.com/anthropics/anthropic-sdk-go",
		Prompt:    "What is this repository about?",
		SessionID: "test-session",
	})
	require.NoError(t, err)
	assert.False(t, resp1.Cached, "first fetch should not be cached")

	// Second fetch — should hit cache.
	resp2, err := manager.Fetch(ctx, &FetchRequest{
		URL:       "https://github.com/anthropics/anthropic-sdk-go",
		Prompt:    "What is this repository about?",
		SessionID: "test-session",
	})
	require.NoError(t, err)
	assert.True(t, resp2.Cached, "second fetch should be cached")
	assert.Equal(t, resp1.Result, resp2.Result, "cached result should match original")

	t.Logf("Cache hit verified: first=%dms second=%dms", resp1.DurationMs, resp2.DurationMs)
}

func TestWebFetchManager_RobotsTxtCompliance(t *testing.T) {
	// Test that robots.txt checking works.
	cfg := DefaultWebFetchConfig()
	cfg.RespectRobotsTxt = true
	cfg.FetchTimeout = 30 * time.Second

	manager, err := NewWebFetchManager(cfg, nil, nil, nil)
	require.NoError(t, err)

	ctx := context.Background()
	err = manager.Start(ctx)
	require.NoError(t, err)
	defer manager.Stop(ctx)

	// GitHub allows crawlers, so this should succeed.
	resp, err := manager.Fetch(ctx, &FetchRequest{
		URL:    "https://github.com/anthropics/anthropic-sdk-go",
		Prompt: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, 200, resp.Code)
}

func truncatePreview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
