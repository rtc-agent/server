package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// WebFetchManager orchestrates the full lifecycle of web content fetching.
// Provides Start/Stop/HealthCheck, consistent with pkg/websearch.WebSearchManager.
type WebFetchManager struct {
	config       WebFetchConfig
	httpClient   *http.Client
	cache        *FetchCache
	security     *SecurityChecker
	domainSem    *DomainSemaphore
	globalSem    chan struct{}
	singleFlight *singleflight.Group
	llmExtractor *LLMExtractor
	rateLimiter  *WebFetchRateLimiter
	robots       *RobotsChecker
	metrics      *FetchMetrics
	logger       *zap.Logger
	auditLogger  *zap.Logger

	started      int32
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

// NewWebFetchManager creates a WebFetchManager. Call Start() after creation.
func NewWebFetchManager(
	config WebFetchConfig,
	redisClient redis.UniversalClient,
	logger *zap.Logger,
	auditLogger *zap.Logger,
) (*WebFetchManager, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if auditLogger == nil {
		auditLogger = zap.NewNop()
	}

	security := NewSecurityChecker(config)
	httpClient := newHTTPClient(config, security)
	cache := NewFetchCache(redisClient)

	// Phase 2: rate limiter and robots.txt checker.
	rateLimiter := NewWebFetchRateLimiter(config.RateLimit)
	var robots *RobotsChecker
	if config.RespectRobotsTxt {
		robots = NewRobotsChecker(config.RobotsCacheTTL, logger)
	}

	m := &WebFetchManager{
		config:       config,
		httpClient:   httpClient,
		cache:        cache,
		security:     security,
		domainSem:    NewDomainSemaphore(config.MaxDomainConcurrency),
		globalSem:    make(chan struct{}, config.MaxConcurrency),
		singleFlight: &singleflight.Group{},
		llmExtractor: nil, // Set via SetLLMExtractor after creation.
		rateLimiter:  rateLimiter,
		robots:       robots,
		metrics:      GetFetchMetrics(),
		logger:       logger,
		auditLogger:  auditLogger,
		shutdownCh:   make(chan struct{}),
	}
	return m, nil
}

// Start launches background goroutines (domain semaphore cleanup, etc.).
func (m *WebFetchManager) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&m.started, 0, 1) {
		return fmt.Errorf("webfetch manager already started")
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error("domain semaphore cleanup goroutine panic recovered",
					zap.Any("panic", r))
			}
		}()
		m.domainSem.cleanupLoop(ctx, m.shutdownCh)
	}()
	m.logger.Info("webfetch manager started")
	return nil
}

// Stop gracefully shuts down the manager.
func (m *WebFetchManager) Stop(ctx context.Context) error {
	m.shutdownOnce.Do(func() {
		close(m.shutdownCh)
		atomic.StoreInt32(&m.started, 0)
		m.logger.Info("webfetch manager stopped")
	})
	return nil
}

// HealthCheck returns nil when the manager is running. Redis failures are
// logged as warnings but do not cause HealthCheck to fail — the system can
// run in degraded mode without caching.
func (m *WebFetchManager) HealthCheck(ctx context.Context) error {
	if atomic.LoadInt32(&m.started) == 0 {
		return fmt.Errorf("webfetch manager not started")
	}
	select {
	case <-m.shutdownCh:
		return fmt.Errorf("webfetch manager is shutting down")
	default:
	}
	if m.cache != nil {
		if err := m.cache.Ping(ctx); err != nil {
			m.logger.Warn("cache health check failed, running in degraded mode (no caching)",
				zap.Error(err))
		}
	}
	return nil
}

// SetLLMExtractor injects an LLM extractor (Phase 2).
func (m *WebFetchManager) SetLLMExtractor(extractor *LLMExtractor) {
	m.llmExtractor = extractor
}

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// FetchRequest is the input to Fetch().
type FetchRequest struct {
	URL       string
	Prompt    string
	SessionID string // used for LLM quota control
}

// FetchResponse is the result of Fetch().
// Output fields (to LLM): URL, Result, Bytes, Code, CodeText, DurationMs.
// Internal fields: Cached, LLMMined, ContentType.
type FetchResponse struct {
	URL         string `json:"url"`
	Result      string `json:"result"`
	Bytes       int64  `json:"bytes"`
	Code        int    `json:"code"`
	CodeText    string `json:"codeText"`
	DurationMs  int64  `json:"durationMs"`
	Cached      bool   `json:"-"`
	LLMMined    bool   `json:"-"`
	ContentType string `json:"-"`
}

// ---------------------------------------------------------------------------
// Fetch — the main entry point
// ---------------------------------------------------------------------------

// Fetch executes the full fetch pipeline:
// validate → cache check → SSRF → concurrency → singleflight → HTTP → process → cache write
func (m *WebFetchManager) Fetch(ctx context.Context, req *FetchRequest) (*FetchResponse, error) {
	start := time.Now()

	// 0. Shutdown check.
	select {
	case <-m.shutdownCh:
		return nil, errors.New("webfetch manager is shutting down")
	default:
	}

	// 1. Validate.
	if err := m.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// 1.5 Pre-parse URL.
	parsedURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	// 2. Cache check (before SSRF to avoid unnecessary DNS).
	cacheKey := m.buildCacheKey(req.URL, req.Prompt)
	if cached, ok := m.cache.Get(ctx, cacheKey); ok {
		m.metrics.RecordCacheHit()
		respCopy := *cached
		respCopy.Cached = true
		respCopy.DurationMs = time.Since(start).Milliseconds()
		m.metrics.RecordSuccess("cached", respCopy.DurationMs)
		m.logAudit(ctx, req, &respCopy, start, nil)
		return &respCopy, nil
	}
	m.metrics.RecordCacheMiss()

	// 3. SSRF check.
	if err := m.security.CheckURL(ctx, req.URL); err != nil {
		m.metrics.RecordSSRFBlocked()
		m.logAudit(ctx, req, nil, start, err)
		return nil, fmt.Errorf("security check failed: %w", err)
	}

	// 3.5 Robots.txt check (Phase 2).
	if m.robots != nil {
		allowed, err := m.robots.IsAllowed(req.URL, m.config.UserAgent)
		if err != nil {
			m.logger.Warn("robots.txt check failed, allowing by default",
				zap.String("url", req.URL), zap.Error(err))
		} else if !allowed {
			m.logAudit(ctx, req, nil, start, fmt.Errorf("blocked by robots.txt"))
			return nil, fmt.Errorf("%w: %s", ErrRobotsBlocked, req.URL)
		}
	}

	// 4. Rate limit.
	if err := m.checkRateLimit(req.URL); err != nil {
		return nil, fmt.Errorf("rate limit exceeded: %w", err)
	}

	// 5. Concurrency control.
	if err := m.acquireSlots(ctx, parsedURL); err != nil {
		return nil, fmt.Errorf("concurrency limit exceeded: %w", err)
	}
	defer m.releaseSlots(parsedURL)

	// 6. singleflight dedup.
	ch := m.singleFlight.DoChan(cacheKey, func() (interface{}, error) {
		rawResult, err := m.doFetch(ctx, req)
		if err != nil {
			if errors.Is(err, ErrCrossDomainRedirect) {
				return rawResult, err
			}
			return nil, err
		}
		response, err := m.processContent(ctx, req, parsedURL, rawResult)
		if err != nil {
			return nil, err
		}
		m.cache.Set(ctx, cacheKey, response, m.config.CacheTTL)
		return response, nil
	})

	// Context-aware wait.
	select {
	case result := <-ch:
		// Cross-domain redirect: build RedirectInfo response.
		if errors.Is(result.Err, ErrCrossDomainRedirect) {
			rawResult, _ := result.Val.(*rawFetchResult)
			redirectURL := ""
			statusCode := 0
			if rawResult != nil {
				redirectURL = rawResult.finalURL
				statusCode = rawResult.statusCode
			}
			redirectResp := &FetchResponse{
				URL:      req.URL,
				Result:   formatRedirectMessage(req.URL, redirectURL, statusCode),
				Code:     statusCode,
				CodeText: http.StatusText(statusCode),
			}
			m.metrics.RecordSuccess("redirect", time.Since(start).Milliseconds())
			m.logAudit(ctx, req, redirectResp, start, nil)
			return redirectResp, nil
		}
		if result.Err != nil {
			m.metrics.RecordError(classifyError(result.Err))
			m.logAudit(ctx, req, nil, start, result.Err)
			return nil, result.Err
		}
		resp := result.Val.(*FetchResponse)
		respCopy := *resp
		respCopy.DurationMs = time.Since(start).Milliseconds()

		domainType := "other"
		if m.security.IsPreApproved(parsedURL.Hostname()) {
			domainType = "pre_approved"
		}
		m.metrics.RecordSuccess(domainType, respCopy.DurationMs)
		m.logAudit(ctx, req, &respCopy, start, nil)
		return &respCopy, nil

	case <-ctx.Done():
		m.metrics.RecordError("context_canceled")
		m.logAudit(ctx, req, nil, start, ctx.Err())
		return nil, fmt.Errorf("fetch canceled: %w", ctx.Err())
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// rawFetchResult holds the raw HTTP response before content processing.
type rawFetchResult struct {
	body        []byte
	contentType string
	charset     string
	statusCode  int
	finalURL    string
}

// validateRequest checks the FetchRequest.
func (m *WebFetchManager) validateRequest(req *FetchRequest) error {
	if req == nil {
		return ErrInvalidURL
	}
	if req.URL == "" {
		return fmt.Errorf("%w: URL is empty", ErrInvalidURL)
	}
	if len(req.URL) > m.config.MaxURLLength {
		return fmt.Errorf("%w: URL exceeds max length (%d > %d)", ErrInvalidURL, len(req.URL), m.config.MaxURLLength)
	}
	return nil
}

// doFetch executes the HTTP request.
func (m *WebFetchManager) doFetch(ctx context.Context, req *FetchRequest) (*rawFetchResult, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, m.config.FetchTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("User-Agent", m.config.UserAgent)
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9,zh;q=0.8")

	resp, err := m.httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, ErrCrossDomainRedirect) && resp != nil {
			return &rawFetchResult{
				statusCode: resp.StatusCode,
				finalURL:   resp.Request.URL.String(),
			}, err
		}
		return nil, m.classifyHTTPError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: HTTP %d", ErrServerError, resp.StatusCode)
		}
		return nil, fmt.Errorf("%w: HTTP %d", ErrClientError, resp.StatusCode)
	}

	limitedReader := io.LimitReader(resp.Body, m.config.MaxContentSize)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return &rawFetchResult{
		body:        body,
		contentType: resp.Header.Get("Content-Type"),
		charset:     detectCharset(resp.Header),
		statusCode:  resp.StatusCode,
		finalURL:    resp.Request.URL.String(),
	}, nil
}

// processContent converts raw bytes to Markdown and optionally runs LLM extraction.
func (m *WebFetchManager) processContent(ctx context.Context, req *FetchRequest, parsedURL *url.URL, raw *rawFetchResult) (*FetchResponse, error) {
	var markdown string
	contentType := raw.contentType

	switch {
	case strings.Contains(raw.contentType, "text/html"):
		body := normalizeToUTF8(raw.body, raw.charset, m.logger)
		var err error
		markdown, err = htmlToMarkdown(body)
		if err != nil {
			m.logger.Warn("html to markdown failed, falling back to plain text", zap.Error(err))
			markdown = stripHTML(body)
			contentType = "text/plain"
		}
	case strings.Contains(raw.contentType, "application/pdf"):
		return nil, fmt.Errorf("PDF extraction not yet implemented (Phase 2)")
	case strings.HasPrefix(raw.contentType, "text/"):
		markdown = string(normalizeToUTF8(raw.body, raw.charset, m.logger))
	default:
		return nil, fmt.Errorf("unsupported content type: %s", raw.contentType)
	}

	// LLM extraction decision.
	llmMined := false
	result := markdown
	host := parsedURL.Hostname()
	isPreApproved := m.security.IsPreApproved(host)

	switch {
	case len(markdown) <= m.config.LLMExtractThresholdBytes:
		result = markdown
	case isPreApproved && len(markdown) <= m.config.LLMExtractThresholdBytes*2:
		result = markdown
	default:
		if m.llmExtractor != nil {
			extracted, err := m.llmExtractor.Extract(ctx, markdown, req.Prompt, isPreApproved, req.SessionID, m.config.MaxLLMExtractPerSession)
			if err != nil {
				m.logger.Warn("LLM extraction failed, returning truncated content", zap.Error(err))
				result = truncateContent(markdown, 100000) + "\n\n[Content truncated due to LLM extraction failure]"
			} else {
				result = extracted
				llmMined = true
			}
		} else {
			result = truncateContent(markdown, 100000) + "\n\n[Content truncated - LLM extraction not configured]"
		}
	}

	return &FetchResponse{
		URL:         req.URL,
		Result:      result,
		ContentType: contentType,
		Bytes:       int64(len(raw.body)),
		Code:        raw.statusCode,
		CodeText:    http.StatusText(raw.statusCode),
		Cached:      false,
		LLMMined:    llmMined,
	}, nil
}

// newHTTPClient builds an HTTP client with SSRF-safe dialer and redirect policy.
func newHTTPClient(config WebFetchConfig, security *SecurityChecker) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	safeDialer := &safeNetDialer{
		dialer:   dialer,
		security: security,
	}
	transport := &http.Transport{
		DialContext:           safeDialer.DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: (&redirectPolicy{config: config}).CheckRedirect,
		Timeout:       config.FetchTimeout,
	}
}

// acquireSlots obtains global + per-domain concurrency slots.
func (m *WebFetchManager) acquireSlots(ctx context.Context, parsedURL *url.URL) error {
	// Global: fail-fast.
	select {
	case m.globalSem <- struct{}{}:
	default:
		m.metrics.RecordConcurrencyLimit("global")
		return fmt.Errorf("%w: global limit (%d)", ErrConcurrencyLimit, m.config.MaxConcurrency)
	}
	// Per-domain: blocking.
	domain := strings.ToLower(parsedURL.Hostname())
	if err := m.domainSem.Acquire(ctx, domain); err != nil {
		<-m.globalSem
		return err
	}
	return nil
}

// releaseSlots frees concurrency slots.
func (m *WebFetchManager) releaseSlots(parsedURL *url.URL) {
	select {
	case <-m.globalSem:
	default:
	}
	domain := strings.ToLower(parsedURL.Hostname())
	m.domainSem.Release(domain)
}

func (m *WebFetchManager) classifyHTTPError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("request canceled: %w", err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("%w: %v", ErrConnectionReset, err)
	}
	return fmt.Errorf("fetch error: %w", err)
}

func (m *WebFetchManager) checkRateLimit(rawURL string) error {
	if m.rateLimiter == nil {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	domain := strings.ToLower(parsed.Hostname())
	if !m.rateLimiter.Allow(domain) {
		m.metrics.RecordRateLimited()
		return fmt.Errorf("%w: domain %s", ErrRateLimited, domain)
	}
	return nil
}

func (m *WebFetchManager) logAudit(ctx context.Context, req *FetchRequest, resp *FetchResponse, start time.Time, fetchErr error) {
	parsedURL, _ := url.Parse(req.URL)
	domain := ""
	if parsedURL != nil {
		domain = parsedURL.Hostname()
	}
	fields := []zap.Field{
		zap.Time("timestamp", start),
		zap.String("session_id", req.SessionID),
		zap.String("url", req.URL),
		zap.String("domain", domain),
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if fetchErr != nil {
		fields = append(fields,
			zap.String("action", "fetch"),
			zap.String("result", "error"),
			zap.String("error_message", fetchErr.Error()),
		)
		m.auditLogger.Warn("web fetch audit", fields...)
		return
	}
	if resp != nil {
		fields = append(fields,
			zap.String("action", "fetch"),
			zap.String("result", "success"),
			zap.Int("status_code", resp.Code),
			zap.Int64("bytes", resp.Bytes),
			zap.Bool("cached", resp.Cached),
			zap.Bool("llm_mined", resp.LLMMined),
		)
	}
	m.auditLogger.Info("web fetch audit", fields...)
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// FormatFetchResponse formats a FetchResponse for LLM consumption.
// Only emits metadata markers when Cached or LLMMined are true.
func FormatFetchResponse(resp *FetchResponse) string {
	var sb strings.Builder
	if resp.Cached || resp.LLMMined {
		sb.WriteString(fmt.Sprintf("URL: %s", resp.URL))
		if resp.Cached {
			sb.WriteString(" [cached]")
		}
		if resp.LLMMined {
			sb.WriteString(" [llm-extracted]")
		}
		sb.WriteString("\n---\n")
	}
	sb.WriteString(resp.Result)
	return sb.String()
}

// formatRedirectMessage builds a cross-domain redirect prompt for the Agent.
func formatRedirectMessage(originalURL, redirectURL string, statusCode int) string {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = fmt.Sprintf("HTTP %d", statusCode)
	}
	return fmt.Sprintf(
		"REDIRECT DETECTED: The URL redirects to a different host.\n\n"+
			"Original URL: %s\n"+
			"Redirect URL: %s\n"+
			"Status: %d %s\n\n"+
			"To complete your request, I need to fetch content from the redirected URL. "+
			"Please use WebFetch again with these parameters:\n"+
			"- url: %q\n"+
			"- prompt: \"<your original prompt>\"",
		originalURL, redirectURL, statusCode, statusText, redirectURL,
	)
}

// truncateContent truncates s to maxLen runes (multi-byte safe).
func truncateContent(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	const suffix = "\n\n[... truncated ...]"
	cut := maxLen - len([]rune(suffix))
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + suffix
}
