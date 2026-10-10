package turnagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// StreamFirstByteTimeout is the maximum time to wait for the first chunk.
// This is typically shorter because if the LLM doesn't start responding
// within 30 seconds, it's likely stuck or there's a connection issue.
const StreamFirstByteTimeout = 30 * time.Second

// StreamIdleTimeout is the maximum time between consecutive chunks.
// This is longer (3 minutes) to accommodate LLM thinking time and
// slow token generation, especially for complex reasoning tasks.
const StreamIdleTimeout = 3 * time.Minute

// consumeStream drives a stream reader to completion.
// The stream is always closed when the function returns, regardless of the exit path.
func (mgr *SessionTurnManager) consumeStream(ctx context.Context, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message]) error {
	defer stream.Close() // Single point of cleanup — prevents resource leaks if new exit paths are added.

	streamCtx, streamSpan := mgr.startSpanIfEnabled(ctx, "turn_agent.consume_stream",
		trace.WithAttributes(
			attribute.String("session.id", mgr.sessionID),
			attribute.String("turn.id", turnID),
			attribute.String("agent.name", agentName),
			attribute.String("message.role", role),
			attribute.String("tool.name", toolName),
		),
	)
	defer streamSpan.End()

	mgr.log(streamCtx, LogLevelDebug, "stream.consume_start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
		"agent_name": agentName,
		"role":       role,
	})

	var maxUsage *schema.TokenUsage
	updateMax := func(usage *schema.TokenUsage) {
		maxUsage = MergeMaxTokenUsage(maxUsage, usage)
	}

	// Accumulate streamed content for lastMessage tracking (Sub Agent support)
	var streamedContent strings.Builder
	var streamedReasoningContent strings.Builder
	chunkCount := 0

	for {
		// Use different timeouts for first chunk vs subsequent chunks:
		// - First chunk: shorter timeout (30s) - if LLM doesn't start quickly, likely stuck
		// - Subsequent chunks: longer timeout (3min) - accommodate thinking/reasoning time
		timeout := StreamIdleTimeout
		if chunkCount == 0 {
			timeout = StreamFirstByteTimeout
		}

		res, timedOut := RecvWithTimeout(streamCtx, stream.Recv, timeout)

		if timedOut {
			streamSpan.RecordError(&StreamIdleTimeoutError{
				SessionID: mgr.sessionID,
				TurnID:    turnID,
				Timeout:   timeout,
				AgentName: agentName,
				Role:      role,
				ToolName:  toolName,
			})
			streamSpan.SetAttributes(
				attribute.Int("stream.chunks", chunkCount),
				attribute.String("stream.status", "idle_timeout"),
			)
			mgr.log(streamCtx, LogLevelError, "stream.idle_timeout", map[string]any{
				"session_id":  mgr.sessionID,
				"turn_id":     turnID,
				"timeout":     timeout.String(),
				"agent_name":  agentName,
				"role":        role,
				"tool_name":   toolName,
				"chunk_count": chunkCount,
			})
			return &StreamIdleTimeoutError{
				SessionID: mgr.sessionID,
				TurnID:    turnID,
				Timeout:   timeout,
				AgentName: agentName,
				Role:      role,
				ToolName:  toolName,
			}
		}

		if res.Err != nil {
			err := mgr.handleStreamRecvError(streamCtx, res.Err, turnID, agentName, role, toolName, maxUsage,
				&streamedContent, &streamedReasoningContent)
			status := "success"
			if err != nil {
				streamSpan.RecordError(err)
				status = "error"
			}
			streamSpan.SetAttributes(
				attribute.Int("stream.chunks", chunkCount),
				attribute.String("stream.status", status),
			)
			return err
		}

		// Defensive: skip nil messages (should not happen in normal operation,
		// but guards against stream reader bugs). Matches summarize.go's nil check.
		if res.Msg == nil {
			continue
		}
		chunkCount++

		var finishReason string
		var tokenUsage *TokenUsage
		if res.Msg.ResponseMeta != nil {
			finishReason = res.Msg.ResponseMeta.FinishReason
			updateMax(res.Msg.ResponseMeta.Usage)
			if res.Msg.ResponseMeta.Usage != nil {
				tokenUsage = extractTokenUsage(res.Msg.ResponseMeta.Usage)
			}
		}
		// Accumulate content for lastMessage tracking
		if res.Msg.Content != "" {
			streamedContent.WriteString(res.Msg.Content)
		}
		if res.Msg.ReasoningContent != "" {
			streamedReasoningContent.WriteString(res.Msg.ReasoningContent)
		}
		if err := mgr.cfg.PublishEvent(streamCtx, mgr.sessionID, turnID, &Event{
			Kind:             EventKindStreamChunk,
			AgentName:        agentName,
			Role:             role,
			ToolName:         toolName,
			Content:          res.Msg.Content,
			ReasoningContent: res.Msg.ReasoningContent,
			FinishReason:     finishReason,
			TokenUsage:       tokenUsage,
		}); err != nil {
			streamSpan.RecordError(err)
			streamSpan.SetAttributes(
				attribute.Int("stream.chunks", chunkCount),
				attribute.String("stream.status", "publish_error"),
			)
			return err
		}
	}
}

// handleStreamRecvError processes errors from stream.Recv, dispatching to the
// appropriate exit path (EOF, cancel, context error, or generic error).
func (mgr *SessionTurnManager) handleStreamRecvError(
	ctx context.Context, recvErr error,
	turnID, agentName, role, toolName string,
	maxUsage *schema.TokenUsage,
	streamedContent, streamedReasoning *strings.Builder,
) error {
	if errors.Is(recvErr, context.Canceled) || errors.Is(recvErr, context.DeadlineExceeded) {
		mgr.log(ctx, LogLevelInfo, "stream.ctx_cancelled", map[string]any{
			"session_id": mgr.sessionID,
			"turn_id":    turnID,
		})
		return recvErr
	}

	if errors.Is(recvErr, io.EOF) {
		// For assistant messages, set lastMessage from accumulated content
		// (needed for Sub Agent support to report the final result).
		// NOTE: lastMessage is only set here on clean EOF, not on stream idle
		// timeout. This is intentional: a timeout indicates an incomplete response,
		// so we don't want to report partial content as the "last message".
		if role == string(schema.Assistant) {
			content := streamedContent.String()
			reasoning := streamedReasoning.String()
			if content != "" || reasoning != "" {
				mgr.setLastMessage(&Message{
					Role:             role,
					Content:          content,
					ReasoningContent: reasoning,
				})
			}
		}
		var aggregatedTokenUsage *TokenUsage
		if maxUsage != nil {
			aggregatedTokenUsage = extractTokenUsage(maxUsage)
		}
		return mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
			Kind:       EventKindStreamEnd,
			AgentName:  agentName,
			Role:       role,
			ToolName:   toolName,
			TokenUsage: aggregatedTokenUsage,
		})
	}

	var cancelErr *adk.CancelError
	if errors.As(recvErr, &cancelErr) {
		return nil
	}

	if pubErr := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
		Kind:      EventKindError,
		AgentName: agentName,
		Err:       recvErr,
	}); pubErr != nil {
		return pubErr
	}
	return recvErr
}

// MergeMaxTokenUsage merges src into dst, keeping the maximum of each field.
// Returns dst (may allocate a new TokenUsage if dst is nil and src is non-nil).
//
// Used in both stream consumption (per-chunk aggregation) and the token
// callback's streaming path (drainStreamAndReport). Exported so that both
// packages share a single implementation instead of duplicating the logic.
func MergeMaxTokenUsage(dst, src *schema.TokenUsage) *schema.TokenUsage {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = &schema.TokenUsage{}
	}
	if src.PromptTokens > dst.PromptTokens {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens > dst.CompletionTokens {
		dst.CompletionTokens = src.CompletionTokens
	}
	if src.TotalTokens > dst.TotalTokens {
		dst.TotalTokens = src.TotalTokens
	}
	if src.PromptTokenDetails.CachedTokens > dst.PromptTokenDetails.CachedTokens {
		dst.PromptTokenDetails.CachedTokens = src.PromptTokenDetails.CachedTokens
	}
	if src.CompletionTokensDetails.ReasoningTokens > dst.CompletionTokensDetails.ReasoningTokens {
		dst.CompletionTokensDetails.ReasoningTokens = src.CompletionTokensDetails.ReasoningTokens
	}
	return dst
}
