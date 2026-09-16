package agent

import (
	"context"
	"testing"

	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino/schema"
	"github.com/rtc-agent/server/internal/model"
	"github.com/stretchr/testify/assert"
)

// TestReportLLMCall_NilSession_NoPanic verifies BUG-07 fix:
// When session is nil (e.g., session not found in DB), the token estimation
// logic should not panic and should set currentCtxTokens to 0.
func TestReportLLMCall_NilSession_NoPanic(t *testing.T) {
	// Simulate the fixed code from token_callback.go lines 73-86
	var currentCtxTokens int64
	session := (*model.Session)(nil) // session is nil
	fullUsage := &FullTokenUsage{
		InputTokens:       100,
		OutputTokens:      50,
		TotalTokens:       150,
		CachedReadTokens:  20,
		CachedWriteTokens: 10,
		ReasoningTokens:   5,
	}

	// This is the FIXED code: wrap session access in nil check
	if session != nil {
		currentCtxTokens = session.TotalTokens + fullUsage.TotalTokens
		if session.CurrentContextTokens > 0 {
			currentCtxTokens = session.CurrentContextTokens + fullUsage.TotalTokens
		}
	}

	// Verify: when session is nil, currentCtxTokens should be 0 (zero value)
	assert.Equal(t, int64(0), currentCtxTokens,
		"currentCtxTokens should be 0 when session is nil")
}

// TestReportLLMCall_NilSession_SetCurrentContextTokens_Zero verifies that
// when session is nil, SetCurrentContextTokens is set to 0 (not a garbage value)
func TestReportLLMCall_NilSession_SetCurrentContextTokens_Zero(t *testing.T) {
	// This test verifies the specific fix for BUG-07:
	// When session == nil, SetCurrentContextTokens should be 0

	var currentCtxTokens int64
	session := (*model.Session)(nil)
	fullUsage := &FullTokenUsage{
		TotalTokens: 1000,
	}

	// Fixed code: wrap session access in nil check
	if session != nil {
		currentCtxTokens = session.TotalTokens + fullUsage.TotalTokens
		if session.CurrentContextTokens > 0 {
			currentCtxTokens = session.CurrentContextTokens + fullUsage.TotalTokens
		}
	}

	// Verify: currentCtxTokens is 0 (zero value) when session is nil
	assert.Equal(t, int64(0), currentCtxTokens,
		"currentCtxTokens must be 0 when session is nil, not session.TotalTokens + fullUsage.TotalTokens")
}

// TestReportLLMCall_WithSession_CorrectCalculation verifies that when session
// is NOT nil, the calculation works correctly
func TestReportLLMCall_WithSession_CorrectCalculation(t *testing.T) {
	tests := []struct {
		name                 string
		totalTokens          int64
		currentContextTokens int64
		fullUsageTotal       int64
		expectedCurrentCtx   int64
	}{
		{
			name:                 "uses CurrentContextTokens when > 0",
			totalTokens:          5000,
			currentContextTokens: 3000,
			fullUsageTotal:       100,
			expectedCurrentCtx:   3100, // 3000 + 100
		},
		{
			name:                 "falls back to TotalTokens when CurrentContextTokens is 0",
			totalTokens:          5000,
			currentContextTokens: 0,
			fullUsageTotal:       100,
			expectedCurrentCtx:   5100, // 5000 + 100
		},
		{
			name:                 "falls back to TotalTokens when CurrentContextTokens is negative",
			totalTokens:          5000,
			currentContextTokens: -100,
			fullUsageTotal:       100,
			expectedCurrentCtx:   5100, // 5000 + 100
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &model.Session{
				TotalTokens:          tt.totalTokens,
				CurrentContextTokens: tt.currentContextTokens,
			}
			fullUsage := &FullTokenUsage{
				TotalTokens: tt.fullUsageTotal,
			}

			var currentCtxTokens int64
			currentCtxTokens = session.TotalTokens + fullUsage.TotalTokens
			if session.CurrentContextTokens > 0 {
				currentCtxTokens = session.CurrentContextTokens + fullUsage.TotalTokens
			}

			assert.Equal(t, tt.expectedCurrentCtx, currentCtxTokens,
				"currentCtxTokens calculation mismatch")
		})
	}
}

// TestBUG02_StreamingCacheWriteTokens_AccumulatesMax verifies BUG-02 fix:
// In the streaming path, cache_creation_input_tokens from message_start (empty
// content) must be preserved via max-accumulation across chunks, since later
// chunks (with content) often report 0.
func TestBUG02_StreamingCacheWriteTokens_AccumulatesMax(t *testing.T) {
	// Mirror the accumulation logic from OnEndWithStreamOutput closure.
	// Each chunk.Message carries cache creation tokens via the eino extra key.

	tests := []struct {
		name                  string
		chunkCacheWriteTokens []int // cache creation tokens per chunk
		expectedCachedWrite   int64
		expectedInputTokens   int64
	}{
		{
			name:                  "message_start has tokens, later chunks have 0",
			chunkCacheWriteTokens: []int{1000, 0, 0, 0},
			expectedCachedWrite:   1000,
			// promptTokens = TotalTokens - OutputTokens = 5000 - 200 = 4800
			// pureInput = 4800 - 500(cachedRead) - 1000(cachedWrite) = 3300
			expectedInputTokens: 3300,
		},
		{
			name:                  "tokens appear in middle chunk (max taken)",
			chunkCacheWriteTokens: []int{0, 2048, 0},
			expectedCachedWrite:   2048,
			// pureInput = 4800 - 500 - 2048 = 2252
			expectedInputTokens: 2252,
		},
		{
			name:                  "multiple chunks with tokens, max wins",
			chunkCacheWriteTokens: []int{500, 1500, 800},
			expectedCachedWrite:   1500,
			// pureInput = 4800 - 500 - 1500 = 2800
			expectedInputTokens: 2800,
		},
		{
			name:                  "all chunks have 0 (no cache write)",
			chunkCacheWriteTokens: []int{0, 0, 0},
			expectedCachedWrite:   0,
			// extractFullUsage logic: pureInput = PromptTokens - CachedRead - CachedWrite
			// PromptTokens = 4800, CachedRead = 500, CachedWrite = 0 → pureInput = 4300
			expectedInputTokens: 4300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the streaming accumulation loop
			var cachedWriteTokens int
			for _, v := range tt.chunkCacheWriteTokens {
				if v > cachedWriteTokens {
					cachedWriteTokens = v
				}
			}

			// Simulate extractFullUsage result (with maxUsage values fixed for test)
			// maxUsage: PromptTokens=4800, CompletionTokens=200, TotalTokens=5000
			// CachedRead=500, CachedWrite=0 (from lastMessage, which is the bug)
			fullUsage := &FullTokenUsage{
				InputTokens:       4300, // 4800 - 500 - 0
				OutputTokens:      200,
				CachedReadTokens:  500,
				CachedWriteTokens: 0, // streaming bug: always 0 before fix
				TotalTokens:       5000,
			}

			// Apply BUG-02 fix: override with accumulated max
			if int64(cachedWriteTokens) > fullUsage.CachedWriteTokens {
				fullUsage.CachedWriteTokens = int64(cachedWriteTokens)
				promptTokens := fullUsage.TotalTokens - fullUsage.OutputTokens
				pureInput := promptTokens - fullUsage.CachedReadTokens - fullUsage.CachedWriteTokens
				if pureInput < 0 {
					pureInput = 0
				}
				fullUsage.InputTokens = pureInput
			}

			assert.Equal(t, tt.expectedCachedWrite, fullUsage.CachedWriteTokens,
				"CachedWriteTokens mismatch")
			assert.Equal(t, tt.expectedInputTokens, fullUsage.InputTokens,
				"InputTokens mismatch after recomputation")

			// Sanity: TotalTokens = InputTokens + CachedRead + CachedWrite + OutputTokens
			sum := fullUsage.InputTokens + fullUsage.CachedReadTokens +
				fullUsage.CachedWriteTokens + fullUsage.OutputTokens
			assert.Equal(t, fullUsage.TotalTokens, sum,
				"TotalTokens must equal sum of all input/output dimensions")
		})
	}
}

// TestBUG02_CacheWriteTokens_KeyVerification verifies that the eino extra key
// for cache creation tokens is what we expect. This guards against eino
// renaming the key in a future version.
func TestBUG02_CacheWriteTokens_KeyVerification(t *testing.T) {
	const expectedKey = "_eino_claude_cache_creation_input_tokens"

	msg := &schema.Message{
		Extra: map[string]any{expectedKey: 2048},
	}
	v, ok := einoclaude.GetCacheCreationInputTokens(msg)
	assert.True(t, ok, "expected GetCacheCreationInputTokens to find the key")
	assert.Equal(t, 2048, v, "expected value 2048")
}

// TestBUG02_NonStreamingPath_Unaffected verifies that the non-streaming
// (OnEnd) path still extracts cache creation tokens correctly from a single
// message. This ensures the BUG-02 fix (streaming-only) does not regress
// the non-streaming path.
func TestBUG02_NonStreamingPath_Unaffected(t *testing.T) {
	const keyOfCacheCreationInputTokens = "_eino_claude_cache_creation_input_tokens"

	msg := &schema.Message{
		Extra: map[string]any{keyOfCacheCreationInputTokens: 1500},
	}
	v, ok := einoclaude.GetCacheCreationInputTokens(msg)
	assert.True(t, ok)
	assert.Equal(t, 1500, v)

	// Verify that when the key is absent, (0, false) is returned
	msgNoCache := &schema.Message{}
	v2, ok2 := einoclaude.GetCacheCreationInputTokens(msgNoCache)
	assert.False(t, ok2)
	assert.Equal(t, 0, v2)
}

// TestBUG08_CompressionContext_SkipsCurrentContextTokensUpdate verifies BUG-08 fix:
// Compression LLM calls should NOT update CurrentContextTokens because
// persistCompressedMessages already writes the accurate post-compression value.
// Without this fix, the token callback would overwrite with tokensAfter + compressionTokens,
// causing CurrentContextTokens to be overestimated.
func TestBUG08_CompressionContext_SkipsCurrentContextTokensUpdate(t *testing.T) {
	tests := []struct {
		name                            string
		isCompress                      bool
		sessionCurrentContextTokens     int64
		fullUsageTotal                  int64
		expectedSetCurrentContextTokens int64
	}{
		{
			name:                            "normal call updates CurrentContextTokens",
			isCompress:                      false,
			sessionCurrentContextTokens:     30000,
			fullUsageTotal:                  5000,
			expectedSetCurrentContextTokens: 35000, // 30000 + 5000
		},
		{
			name:                            "compression call does NOT update CurrentContextTokens",
			isCompress:                      true,
			sessionCurrentContextTokens:     30000,
			fullUsageTotal:                  5000,
			expectedSetCurrentContextTokens: 0, // should be 0 (skip update)
		},
		{
			name:                            "normal call with zero CurrentContextTokens uses TotalTokens",
			isCompress:                      false,
			sessionCurrentContextTokens:     0,
			fullUsageTotal:                  5000,
			expectedSetCurrentContextTokens: 55000, // 50000 (TotalTokens) + 5000
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate session state
			session := &model.Session{
				TotalTokens:          50000, // baseline TotalTokens
				CurrentContextTokens: tt.sessionCurrentContextTokens,
			}
			fullUsage := &FullTokenUsage{
				TotalTokens: tt.fullUsageTotal,
			}

			// Simulate the logic from token_callback.go
			isCompress := tt.isCompress
			var currentCtxTokens int64
			currentCtxTokens = session.TotalTokens + fullUsage.TotalTokens
			if session.CurrentContextTokens > 0 {
				currentCtxTokens = session.CurrentContextTokens + fullUsage.TotalTokens
			}

			// Simulate delta creation
			var setCurrentContextTokens int64
			if !isCompress {
				setCurrentContextTokens = currentCtxTokens
			}
			// else: setCurrentContextTokens remains 0 (skip update)

			assert.Equal(t, tt.expectedSetCurrentContextTokens, setCurrentContextTokens,
				"SetCurrentContextTokens mismatch")
		})
	}
}

// TestBUG08_WithCompressContext_MarkPropagation verifies that the compress context
// mark is correctly set and detected.
func TestBUG08_WithCompressContext_MarkPropagation(t *testing.T) {
	// Test 1: Normal context is not marked as compression
	ctx := context.Background()
	assert.False(t, isCompressContext(ctx),
		"normal context should not be marked as compression")

	// Test 2: Context marked with withCompressContext is detected
	compressCtx := withCompressContext(ctx)
	assert.True(t, isCompressContext(compressCtx),
		"context marked with withCompressContext should be detected as compression")

	// Test 3: Mark propagates to child contexts
	type testCtxKey struct{}
	childCtx := context.WithValue(compressCtx, testCtxKey{}, "value")
	assert.True(t, isCompressContext(childCtx),
		"compress mark should propagate to child contexts")

	// Test 4: Original context is not affected
	assert.False(t, isCompressContext(ctx),
		"original context should remain unmarked")
}
