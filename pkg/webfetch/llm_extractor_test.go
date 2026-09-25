package webfetch

import (
	"context"
	"fmt"
	"testing"
)

// mockLLMClient implements LLMClient for testing.
type mockLLMClient struct {
	response string
	err      error
}

func (m *mockLLMClient) Generate(ctx context.Context, prompt string, maxTokens int, sessionID string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

func TestLLMExtractor_Extract_Success(t *testing.T) {
	client := &mockLLMClient{response: "Extracted content"}
	extractor := NewLLMExtractor(client, nil, 4096)

	result, err := extractor.Extract(context.Background(), "large content", "extract info", true, "session1", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Extracted content" {
		t.Errorf("expected 'Extracted content', got %q", result)
	}
}

func TestLLMExtractor_Extract_NilExtractor(t *testing.T) {
	var extractor *LLMExtractor
	_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 10)
	if err == nil {
		t.Fatal("expected error for nil extractor")
	}
}

func TestLLMExtractor_Extract_NilClient(t *testing.T) {
	extractor := &LLMExtractor{client: nil}
	_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 10)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestLLMExtractor_Extract_ClientError(t *testing.T) {
	client := &mockLLMClient{err: fmt.Errorf("LLM error")}
	extractor := NewLLMExtractor(client, nil, 4096)

	_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 10)
	if err == nil {
		t.Fatal("expected error from client")
	}
}

func TestLLMExtractor_QuotaEnforcement(t *testing.T) {
	client := &mockLLMClient{response: "ok"}
	extractor := NewLLMExtractor(client, nil, 4096)

	// First 3 calls should succeed (limit = 3).
	for i := 0; i < 3; i++ {
		_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 3)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	// 4th call should fail (quota exceeded).
	_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 3)
	if err == nil {
		t.Fatal("expected quota exceeded error")
	}
}

func TestLLMExtractor_QuotaReset(t *testing.T) {
	client := &mockLLMClient{response: "ok"}
	extractor := NewLLMExtractor(client, nil, 4096)

	// Use up quota.
	for i := 0; i < 2; i++ {
		_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 2)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	// Quota exhausted.
	_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 2)
	if err == nil {
		t.Fatal("expected quota exceeded error")
	}

	// Reset quota.
	extractor.ResetSessionQuota("session1")

	// Should succeed again.
	_, err = extractor.Extract(context.Background(), "content", "prompt", true, "session1", 2)
	if err != nil {
		t.Fatalf("unexpected error after reset: %v", err)
	}
}

func TestLLMExtractor_NoQuotaLimit(t *testing.T) {
	client := &mockLLMClient{response: "ok"}
	extractor := NewLLMExtractor(client, nil, 4096)

	// maxPerSession = 0 means unlimited.
	for i := 0; i < 100; i++ {
		_, err := extractor.Extract(context.Background(), "content", "prompt", true, "session1", 0)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
}

func TestMakeSecondaryModelPrompt_PreApproved(t *testing.T) {
	prompt := makeSecondaryModelPrompt("content", "extract info", true)
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	// Pre-approved domains should have more generous guidelines.
	if !contains(prompt, "Include relevant details, code examples") {
		t.Error("expected pre-approved domain guidelines")
	}
}

func TestMakeSecondaryModelPrompt_NonPreApproved(t *testing.T) {
	prompt := makeSecondaryModelPrompt("content", "extract info", false)
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	// Non-pre-approved domains should have stricter guidelines.
	if !contains(prompt, "strict 125-character maximum") {
		t.Error("expected non-pre-approved domain guidelines")
	}
}

func TestMakeSecondaryModelPrompt_ContentTruncation(t *testing.T) {
	// Create content larger than LLMInputHardLimitChars.
	largeContent := make([]byte, LLMInputHardLimitChars+1000)
	for i := range largeContent {
		largeContent[i] = 'a'
	}

	prompt := makeSecondaryModelPrompt(string(largeContent), "extract", true)
	// Should contain truncation marker.
	if !contains(prompt, "[Content truncated due to LLM input limit]") {
		t.Error("expected truncation marker for oversized content")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
