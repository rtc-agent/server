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

	"github.com/leichujun/rtc-agent/server/pkg/circuitbreaker"
	"github.com/leichujun/rtc-agent/server/pkg/proxy"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// WebFetchManager orchestrates the full lifecycle of web content fetching.
// Provides Start/Stop/HealthCheck, consistent with pkg/websearch.WebSearchManager.
type WebFetchManager struct {
	config         WebFetchConfig
	httpClient     *http.Client
	cache          *FetchCache
	security       *SecurityChecker
	domainSem      *DomainSemaphore
	globalSem      chan struct{}
	singleFlight   *singleflight.Group
	llmExtractor   LLMExtractor
	llmResultCache *LLMResultCache
	cacheSync      *DistributedCacheSync
	pdfExtractor   *pdfExtractor
	rateLimiter    *WebFetchRateLimiter
	robots         *RobotsChecker
	breakers       map[string]*circuitbreaker.CircuitBreaker // per-domain circuit breakers
	breakersMu     sync.RWMutex
	proxyPool      *proxy.ProxyPool
	metrics        *FetchMetrics
	logger         *zap.Logger
	auditLogger    *zap.Logger

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

	// Phase 3: circuit breakers and proxy pool.
	var proxyPool *proxy.ProxyPool
	if len(config.Proxies) > 0 {
		proxyPool = proxy.NewProxyPool(config.Proxies, config.ProxyHealthURL, config.ProxyCheckInterval, logger)
	}

	// Phase 3: LLM result cache.
	llmResultCache := NewLLMResultCache(redisClient, config.LLMCacheTTL)

	// Phase 3: Distributed cache sync.
	var cacheSync *DistributedCacheSync
	if config.CacheSyncEnabled {
		cacheSync = NewDistributedCacheSync(redisClient, config.CacheSyncChannel, config.CacheSyncSourceID, logger)
	}

	m := &WebFetchManager{
		config:         config,
		httpClient:     httpClient,
		cache:          cache,
		security:       security,
		domainSem:      NewDomainSemaphore(config.MaxDomainConcurrency),
		globalSem:      make(chan struct{}, config.MaxConcurrency),
		singleFlight:   &singleflight.Group{},
		llmExtractor:   nil, // Set via SetLLMExtractor after creation.
		llmResultCache: llmResultCache,
		cacheSync:      cacheSync,
		pdfExtractor:   newPDFExtractor(logger),
		rateLimiter:    rateLimiter,
		robots:         robots,
		breakers:       make(map[string]*circuitbreaker.CircuitBreaker),
		proxyPool:      proxyPool,
		metrics:        GetFetchMetrics(),
		logger:         logger,
		auditLogger:    auditLogger,
		shutdownCh:     make(chan struct{}),
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
	if m.proxyPool != nil {
		m.proxyPool.Start()
	}
	if m.cacheSync != nil {
		if err := m.cacheSync.Start(ctx); err != nil {
			m.logger.Warn("failed to start distributed cache sync", zap.Error(err))
		} else {
			m.logger.Warn("distributed cache sync started but is NON-FUNCTIONAL - feature incomplete, see distributed_cache_sync.go")
			// Register callbacks to sync cache operations from other instances.
			m.cacheSync.OnSet(func(ctx context.Context, key, value string) {
				// Sync cache set from other instance.
				// Note: We need to deserialize the FetchResponse from the value.
				// For simplicity, we'll skip the actual sync logic here since it requires
				// the cache implementation details. In production, this would deserialize
				// and set the cache entry.
				m.logger.Debug("cache sync: set from remote (NOT ACTUALLY SYNCED)", zap.String("key", key))
			})
			m.cacheSync.OnDelete(func(ctx context.Context, key string) {
				// Sync cache delete from other instance.
				m.logger.Debug("cache sync: delete from remote", zap.String("key", key))
			})
		}
	}
	m.logger.Info("webfetch manager started")
	return nil
}

// Stop gracefully shuts down the manager.
func (m *WebFetchManager) Stop(ctx context.Context) error {
	m.shutdownOnce.Do(func() {
		close(m.shutdownCh)
		atomic.StoreInt32(&m.started, 0)
		if m.proxyPool != nil {
			m.proxyPool.Stop()
		}
		if m.cacheSync != nil {
			if err := m.cacheSync.Stop(); err != nil {
				m.logger.Warn("failed to stop distributed cache sync", zap.Error(err))
			}
		}
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

// SetLLMExtractor injects an LLM extractor.
// The implementation lives in internal/agent layer.
func (m *WebFetchManager) SetLLMExtractor(extractor LLMExtractor) {
	m.llmExtractor = extractor
}

// getOrCreateBreaker returns a circuit breaker for the given domain.
// Creates one lazily if circuit breaker is enabled and doesn't exist yet.
func (m *WebFetchManager) getOrCreateBreaker(domain string) *circuitbreaker.CircuitBreaker {
	if !m.config.CircuitBreaker.Enabled {
		return nil
	}

	m.breakersMu.RLock()
	cb, ok := m.breakers[domain]
	m.breakersMu.RUnlock()
	if ok {
		return cb
	}

	m.breakersMu.Lock()
	defer m.breakersMu.Unlock()

	// Double-check after acquiring write lock.
	if cb, ok := m.breakers[domain]; ok {
		return cb
	}

	cbCfg := circuitbreaker.CircuitBreakerConfig{
		FailureThreshold:    m.config.CircuitBreaker.FailureThreshold,
		OpenTimeout:         m.config.CircuitBreaker.OpenTimeout,
		HalfOpenMaxRequests: m.config.CircuitBreaker.HalfOpenMaxRequests,
		WindowSize:          m.config.CircuitBreaker.WindowSize,
		WindowDuration:      m.config.CircuitBreaker.WindowDuration,
	}
	cb = circuitbreaker.NewCircuitBreaker(cbCfg, domain)
	m.breakers[domain] = cb
	return cb
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

		// Phase 3: Publish cache set to other instances.
		if m.cacheSync != nil {
			// Note: We need to serialize the response for the sync message.
			// For simplicity, we'll just publish the key. In production, this would
			// serialize the full response and publish it.
			if err := m.cacheSync.PublishSet(ctx, cacheKey, ""); err != nil {
				m.logger.Warn("failed to publish cache sync", zap.Error(err))
			}
		}

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
		resp, ok := result.Val.(*FetchResponse)
		if !ok || resp == nil {
			return nil, fmt.Errorf("unexpected result type from singleflight: %T", result.Val)
		}
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

	// Get circuit breaker for this domain (if enabled).
	parsedURL, _ := url.Parse(req.URL)
	domain := ""
	if parsedURL != nil {
		domain = parsedURL.Hostname()
	}
	cb := m.getOrCreateBreaker(domain)

	// Execute HTTP request through circuit breaker (if enabled).
	var rawResult *rawFetchResult
	var fetchErr error

	if cb != nil {
		// Use circuit breaker to protect the request.
		result, err := cb.Execute(func() (interface{}, error) {
			return m.executeHTTPRequest(fetchCtx, req)
		})
		if err != nil {
			fetchErr = err
		} else if result != nil {
			var ok bool
			rawResult, ok = result.(*rawFetchResult)
			if !ok {
				fetchErr = fmt.Errorf("unexpected result type from circuit breaker: %T", result)
			}
		}
	} else {
		// No circuit breaker, execute directly.
		rawResult, fetchErr = m.executeHTTPRequest(fetchCtx, req)
	}

	if fetchErr != nil {
		if errors.Is(fetchErr, ErrCrossDomainRedirect) && rawResult != nil {
			return rawResult, fetchErr
		}
		return nil, m.classifyHTTPError(fetchErr)
	}

	return rawResult, nil
}

// executeHTTPRequest performs the actual HTTP request (extracted for circuit breaker wrapping).
func (m *WebFetchManager) executeHTTPRequest(ctx context.Context, req *FetchRequest) (*rawFetchResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
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
		return nil, err
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
		// Phase 3: PDF extraction.
		var err error
		markdown, err = m.pdfExtractor.extractText(raw.body)
		if err != nil {
			m.logger.Warn("PDF extraction failed", zap.Error(err))
			return nil, fmt.Errorf("PDF extraction failed: %w", err)
		}
		contentType = "text/plain"
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
			// Phase 3: Check LLM result cache first.
			if m.llmResultCache != nil {
				if cached, ok := m.llmResultCache.Get(ctx, markdown, req.Prompt); ok {
					m.logger.Debug("LLM result cache hit",
						zap.String("url", req.URL),
						zap.Int("content_len", len(markdown)))
					result = cached
					llmMined = true
					break
				}
			}

			// Cache miss: call LLM.
			extracted, err := m.llmExtractor.Extract(ctx, markdown, req.Prompt, isPreApproved, req.SessionID)
			if err != nil {
				m.logger.Warn("LLM extraction failed, returning truncated content", zap.Error(err))
				result = truncateContent(markdown, 100000) + "\n\n[Content truncated due to LLM extraction failure]"
			} else {
				result = extracted
				llmMined = true

				// Phase 3: Store LLM result in cache.
				if m.llmResultCache != nil {
					m.llmResultCache.Set(ctx, markdown, req.Prompt, extracted)
					m.logger.Debug("LLM result cached",
						zap.String("url", req.URL),
						zap.Int("content_len", len(markdown)),
						zap.Int("result_len", len(extracted)))
				}
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
