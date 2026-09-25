package agent

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rtc-agent/server/internal/agent/templateutil"
	"github.com/rtc-agent/server/pkg/webfetch"
	"go.uber.org/zap"
)

//go:embed prompts/outputs/webfetch-extract.md.tmpl
var webfetchExtractTmpl string

// WebFetchLLMExtractor implements webfetch.LLMExtractor interface.
// Manages LLM-based content extraction with per-session quotas.
type WebFetchLLMExtractor struct {
	client        webfetch.LLMClient
	logger        *zap.Logger
	maxTokens     int
	maxPerSession int

	// Per-session daily quota tracking.
	mu            sync.Mutex
	sessionCounts map[string]*int64 // sessionID -> pointer to count
}

// NewWebFetchLLMExtractor creates a new WebFetchLLMExtractor.
func NewWebFetchLLMExtractor(
	client webfetch.LLMClient,
	logger *zap.Logger,
	maxTokens int,
	maxPerSession int,
) *WebFetchLLMExtractor {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WebFetchLLMExtractor{
		client:        client,
		logger:        logger,
		maxTokens:     maxTokens,
		maxPerSession: maxPerSession,
		sessionCounts: make(map[string]*int64),
	}
}

// Extract performs LLM-based content extraction.
func (e *WebFetchLLMExtractor) Extract(
	ctx context.Context,
	content, prompt string,
	isPreapprovedDomain bool,
	sessionID string,
) (string, error) {
	if e == nil || e.client == nil {
		return "", fmt.Errorf("LLM extractor not configured")
	}

	// Check per-session quota.
	if sessionID != "" && e.maxPerSession > 0 {
		if !e.checkQuota(sessionID) {
			return "", fmt.Errorf("LLM extraction quota exceeded for session %s (limit: %d/day)", sessionID, e.maxPerSession)
		}
	}

	// Build extraction prompt using template.
	extractionPrompt := e.buildPrompt(content, prompt, isPreapprovedDomain)

	result, err := e.client.Generate(ctx, extractionPrompt, e.maxTokens, sessionID)
	if err != nil {
		return "", fmt.Errorf("LLM extraction failed: %w", err)
	}

	// Increment quota counter on success.
	if sessionID != "" && e.maxPerSession > 0 {
		e.incrementQuota(sessionID)
	}

	return result, nil
}

// buildPrompt constructs the extraction prompt using the embedded template.
func (e *WebFetchLLMExtractor) buildPrompt(content, prompt string, isPreapprovedDomain bool) string {
	// Truncate content if it exceeds the hard limit.
	if len([]rune(content)) > webfetch.LLMInputHardLimitChars {
		content = string([]rune(content)[:webfetch.LLMInputHardLimitChars]) + "\n\n[Content truncated due to LLM input limit]"
	}

	return templateutil.MustRender("webfetch-extract", webfetchExtractTmpl, map[string]any{
		"Content":             content,
		"Prompt":              prompt,
		"IsPreapprovedDomain": isPreapprovedDomain,
	})
}

// checkQuota returns true if the session has remaining LLM extraction quota.
func (e *WebFetchLLMExtractor) checkQuota(sessionID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	countPtr, ok := e.sessionCounts[sessionID]
	if !ok {
		return true // no count yet, first call
	}
	return atomic.LoadInt64(countPtr) < int64(e.maxPerSession)
}

// incrementQuota increments the session's LLM extraction counter.
func (e *WebFetchLLMExtractor) incrementQuota(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	countPtr, ok := e.sessionCounts[sessionID]
	if !ok {
		var count int64
		e.sessionCounts[sessionID] = &count
		countPtr = &count
	}
	atomic.AddInt64(countPtr, 1)
}

// ResetSessionQuota clears the LLM extraction quota for a session.
// Called when a new day starts or when explicitly resetting.
func (e *WebFetchLLMExtractor) ResetSessionQuota(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessionCounts, sessionID)
}
