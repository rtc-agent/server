package webfetch

import (
	"context"
)

// LLMInputHardLimitChars is the hard upper bound for LLM input (~50K tokens).
// Content exceeding this is truncated before being sent to the model.
const LLMInputHardLimitChars = 200_000

// LLMClient is the interface for LLM generation calls.
// Implemented by the server layer (wrapping eino ChatModel or anthropic SDK).
// Defined here (in the webfetch module) to avoid cross-module dependencies.
type LLMClient interface {
	Generate(ctx context.Context, prompt string, maxTokens int, sessionID string) (string, error)
}

// LLMExtractor is the interface for LLM-based content extraction.
// The implementation lives in internal/agent layer to centralize prompt management.
type LLMExtractor interface {
	// Extract performs LLM-based content extraction.
	// content: the markdown content to extract from
	// prompt: the user's extraction prompt
	// isPreapprovedDomain: whether the domain is pre-approved (affects extraction strictness)
	// sessionID: for quota tracking
	// Returns the extracted/summarized text.
	Extract(ctx context.Context, content, prompt string, isPreapprovedDomain bool, sessionID string) (string, error)
}
