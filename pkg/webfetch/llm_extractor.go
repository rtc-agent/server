package webfetch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// LLMInputHardLimitChars is the hard upper bound for LLM input (~50K tokens).
// Content exceeding this is truncated before being sent to the model.
const LLMInputHardLimitChars = 200_000

// LLMClient is the interface for LLM extraction calls.
// Implemented by the server layer (wrapping eino ChatModel or anthropic SDK).
// Defined here (in the webfetch module) to avoid cross-module dependencies.
// The sessionID is passed for token tracking and metrics.
type LLMClient interface {
	Generate(ctx context.Context, prompt string, maxTokens int, sessionID string) (string, error)
}

// LLMExtractor uses an LLM to intelligently extract information from large content.
// It enforces per-session daily quotas and input size limits.
type LLMExtractor struct {
	client    LLMClient
	logger    *zap.Logger
	maxTokens int

	// Per-session daily quota tracking.
	mu             sync.Mutex
	sessionCounts  map[string]*int64 // sessionID -> pointer to count
}

// NewLLMExtractor creates an LLMExtractor with a client implementation.
func NewLLMExtractor(client LLMClient, logger *zap.Logger, maxTokens int) *LLMExtractor {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &LLMExtractor{
		client:        client,
		logger:        logger,
		maxTokens:     maxTokens,
		sessionCounts: make(map[string]*int64),
	}
}

// Extract runs LLM-based content extraction.
// Returns the extracted/summarized text.
// isPreapprovedDomain controls the extraction strictness (pre-approved domains
// get more generous quoting allowances).
// maxPerSession limits daily LLM extraction calls per session; 0 means unlimited.
func (e *LLMExtractor) Extract(ctx context.Context, content, prompt string, isPreapprovedDomain bool, sessionID string, maxPerSession int) (string, error) {
	if e == nil || e.client == nil {
		return "", fmt.Errorf("LLM extractor not configured")
	}

	// Check per-session quota.
	if sessionID != "" && maxPerSession > 0 {
		if !e.checkQuota(sessionID, maxPerSession) {
			return "", fmt.Errorf("LLM extraction quota exceeded for session %s (limit: %d/day)", sessionID, maxPerSession)
		}
	}

	extractionPrompt := makeSecondaryModelPrompt(content, prompt, isPreapprovedDomain)

	result, err := e.client.Generate(ctx, extractionPrompt, e.maxTokens, sessionID)
	if err != nil {
		return "", fmt.Errorf("LLM extraction failed: %w", err)
	}

	// Increment quota counter on success.
	if sessionID != "" && maxPerSession > 0 {
		e.incrementQuota(sessionID)
	}

	return result, nil
}

// checkQuota returns true if the session has remaining LLM extraction quota.
func (e *LLMExtractor) checkQuota(sessionID string, maxPerSession int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	countPtr, ok := e.sessionCounts[sessionID]
	if !ok {
		return true // no count yet, first call
	}
	return atomic.LoadInt64(countPtr) < int64(maxPerSession)
}

// incrementQuota increments the session's LLM extraction counter.
func (e *LLMExtractor) incrementQuota(sessionID string) {
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
func (e *LLMExtractor) ResetSessionQuota(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessionCounts, sessionID)
}

// makeSecondaryModelPrompt builds the prompt for LLM extraction.
func makeSecondaryModelPrompt(markdownContent, prompt string, isPreapprovedDomain bool) string {
	content := markdownContent
	if len([]rune(content)) > LLMInputHardLimitChars {
		content = string([]rune(content)[:LLMInputHardLimitChars]) + "\n\n[Content truncated due to LLM input limit]"
	}

	var guidelines string
	if isPreapprovedDomain {
		guidelines = `Provide a concise response based on the content above. Include relevant details, code examples, and documentation excerpts as needed.

MULTILINGUAL GUIDANCE: If the content contains non-English text, preserve key technical terms in their original language while providing the response in the language requested by the user's prompt.`
	} else {
		guidelines = `Provide a concise response based only on the content above. In your response:
 - Enforce a strict 125-character maximum for quotes from any source document.
 - Use quotation marks for exact language from articles; any language outside of the quotation should never be word-for-word the same.

MULTILINGUAL GUIDANCE: If the content contains non-English text, preserve key technical terms in their original language while providing the response in the language requested by the user's prompt.`
	}

	return fmt.Sprintf(`Web page content:
---
%s
---

%s

%s`, content, prompt, guidelines)
}
