package turnagent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSummarizationMiddleware_OnCompress_SkippedWhenNoCompression verifies BUG-10 fix:
// When compressContext returns the same slice (no compression happened),
// OnCompress should NOT be called to avoid data corruption.
func TestSummarizationMiddleware_OnCompress_SkippedWhenNoCompression(t *testing.T) {
	onCompressCalled := false
	var capturedCompressed []*schema.Message

	mw, err := NewSummarizationMiddleware(&SummarizationConfig{
		Trigger: &TriggerCondition{ContextTokens: 100},
		TokenCounter: func(ctx context.Context, messages []*schema.Message) (int, error) {
			// Return a value that triggers compression
			return 200, nil
		},
		CompressContext: func(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error) {
			// Simulate compression being skipped: return the same slice
			return messages, nil
		},
		OnCompress: func(ctx context.Context, compressed []*schema.Message) error {
			onCompressCalled = true
			capturedCompressed = compressed
			return nil
		},
	})
	require.NoError(t, err)

	// Create test messages
	messages := []*schema.Message{
		{Role: schema.User, Content: "message 1"},
		{Role: schema.Assistant, Content: "response 1"},
	}

	// Create a state with these messages
	state := &adk.ChatModelAgentState{
		Messages: messages,
	}

	// Execute the middleware
	ctx := context.Background()
	_, resultState, err := mw.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)

	// Verify: OnCompress should NOT be called when compression was skipped
	assert.False(t, onCompressCalled,
		"OnCompress should NOT be called when CompressContext returns the same slice")
	assert.Nil(t, capturedCompressed,
		"compressed should be nil when OnCompress is not called")

	// Verify: state.Messages should remain unchanged
	assert.Equal(t, len(messages), len(resultState.Messages),
		"state.Messages should have the same length")
}

// TestSummarizationMiddleware_OnCompress_CalledWhenCompressionHappens verifies
// that OnCompress IS called when actual compression occurs
func TestSummarizationMiddleware_OnCompress_CalledWhenCompressionHappens(t *testing.T) {
	onCompressCalled := false
	var capturedCompressed []*schema.Message

	mw, err := NewSummarizationMiddleware(&SummarizationConfig{
		Trigger: &TriggerCondition{ContextTokens: 100},
		TokenCounter: func(ctx context.Context, messages []*schema.Message) (int, error) {
			return 200, nil
		},
		CompressContext: func(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error) {
			// Simulate actual compression: return a NEW slice
			compressed := []*schema.Message{
				{Role: schema.User, Content: "summary"},
			}
			return compressed, nil
		},
		OnCompress: func(ctx context.Context, compressed []*schema.Message) error {
			onCompressCalled = true
			capturedCompressed = compressed
			return nil
		},
	})
	require.NoError(t, err)

	messages := []*schema.Message{
		{Role: schema.User, Content: "message 1"},
		{Role: schema.Assistant, Content: "response 1"},
		{Role: schema.User, Content: "message 2"},
		{Role: schema.Assistant, Content: "response 2"},
	}

	state := &adk.ChatModelAgentState{
		Messages: messages,
	}
	ctx := context.Background()
	_, resultState, err := mw.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)

	// Verify: OnCompress SHOULD be called when compression happens
	assert.True(t, onCompressCalled,
		"OnCompress SHOULD be called when CompressContext returns a different slice")
	require.NotNil(t, capturedCompressed,
		"compressed should not be nil when OnCompress is called")
	assert.Equal(t, 1, len(capturedCompressed),
		"compressed should have 1 message (the summary)")
	assert.Equal(t, "summary", capturedCompressed[0].Content,
		"compressed message should be the summary")

	// Verify: state.Messages should be updated to compressed
	assert.Equal(t, 1, len(resultState.Messages),
		"state.Messages should be updated to compressed")
	assert.Equal(t, "summary", resultState.Messages[0].Content,
		"state.Messages should contain the summary")
}

// TestSummarizationMiddleware_LogFields_CorrectOriginalCount verifies BUG-12 fix:
// The log field "original_messages_count" should reflect the count BEFORE reassignment
func TestSummarizationMiddleware_LogFields_CorrectOriginalCount(t *testing.T) {
	var logAttrs map[string]any

	mw, err := NewSummarizationMiddleware(&SummarizationConfig{
		Trigger: &TriggerCondition{ContextTokens: 100},
		TokenCounter: func(ctx context.Context, messages []*schema.Message) (int, error) {
			return 200, nil
		},
		CompressContext: func(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error) {
			// Compress 4 messages into 1
			return []*schema.Message{
				{Role: schema.User, Content: "summary"},
			}, nil
		},
		Log: &testLogger{
			debugFunc: func(ctx context.Context, msg string, attrs map[string]any) {
				if msg == "compress.state_updated" {
					logAttrs = attrs
				}
			},
		},
	})
	require.NoError(t, err)

	// Create 4 original messages
	messages := []*schema.Message{
		{Role: schema.User, Content: "message 1"},
		{Role: schema.Assistant, Content: "response 1"},
		{Role: schema.User, Content: "message 2"},
		{Role: schema.Assistant, Content: "response 2"},
	}

	state := &adk.ChatModelAgentState{
		Messages: messages,
	}
	ctx := context.Background()
	_, _, err = mw.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)

	// Verify: log fields should show correct counts
	require.NotNil(t, logAttrs, "compress.state_updated log should be emitted")

	compressedCount, ok := logAttrs["compressed_messages_count"].(int)
	require.True(t, ok, "compressed_messages_count should be an int")
	assert.Equal(t, 1, compressedCount,
		"compressed_messages_count should be 1 (the summary)")

	originalCount, ok := logAttrs["original_messages_count"].(int)
	require.True(t, ok, "original_messages_count should be an int")
	assert.Equal(t, 4, originalCount,
		"original_messages_count should be 4 (before compression), not 1 (after)")
}

// TestSummarizationMiddleware_CompressionSkipped_EmptyMessages verifies that
// when messages are empty, compression is handled correctly
func TestSummarizationMiddleware_CompressionSkipped_EmptyMessages(t *testing.T) {
	onCompressCalled := false

	mw, err := NewSummarizationMiddleware(&SummarizationConfig{
		Trigger: &TriggerCondition{ContextTokens: 100},
		TokenCounter: func(ctx context.Context, messages []*schema.Message) (int, error) {
			return 200, nil
		},
		CompressContext: func(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error) {
			// Return empty slice (same as input)
			return messages, nil
		},
		OnCompress: func(ctx context.Context, compressed []*schema.Message) error {
			onCompressCalled = true
			return nil
		},
	})
	require.NoError(t, err)

	// Empty messages
	messages := []*schema.Message{}
	state := &adk.ChatModelAgentState{
		Messages: messages,
	}

	ctx := context.Background()
	_, _, err = mw.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)

	// Verify: OnCompress should NOT be called for empty messages
	assert.False(t, onCompressCalled,
		"OnCompress should NOT be called when messages are empty")
}

// testLogger is a minimal logger implementation for testing
type testLogger struct {
	debugFunc func(ctx context.Context, msg string, attrs map[string]any)
	infoFunc  func(ctx context.Context, msg string, attrs map[string]any)
	warnFunc  func(ctx context.Context, msg string, attrs map[string]any)
	errorFunc func(ctx context.Context, msg string, attrs map[string]any)
}

func (l *testLogger) Debug(ctx context.Context, msg string, attrs map[string]any) {
	if l.debugFunc != nil {
		l.debugFunc(ctx, msg, attrs)
	}
}

func (l *testLogger) Info(ctx context.Context, msg string, attrs map[string]any) {
	if l.infoFunc != nil {
		l.infoFunc(ctx, msg, attrs)
	}
}

func (l *testLogger) Warn(ctx context.Context, msg string, attrs map[string]any) {
	if l.warnFunc != nil {
		l.warnFunc(ctx, msg, attrs)
	}
}

func (l *testLogger) Error(ctx context.Context, msg string, attrs map[string]any) {
	if l.errorFunc != nil {
		l.errorFunc(ctx, msg, attrs)
	}
}
