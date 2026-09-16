package turnagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// StreamIdleTimeout is the maximum duration a stream can remain idle
// (no data received) before it is considered stalled and closed.
// This prevents turns from getting stuck indefinitely when the LLM
// stream stops mid-response.
const StreamIdleTimeout = 3 * time.Minute

// consumeStream drives a stream reader to completion.
// The stream is always closed when the function returns, regardless of the exit path.
func (mgr *SessionTurnManager) consumeStream(ctx context.Context, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message]) error {
	defer stream.Close() // Single point of cleanup — prevents resource leaks if new exit paths are added.

	mgr.log(ctx, LogLevelDebug, "stream.consume_start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
		"agent_name": agentName,
		"role":       role,
	})

	var maxUsage *schema.TokenUsage
	updateMax := func(usage *schema.TokenUsage) {
		maxUsage = mergeMaxTokenUsage(maxUsage, usage)
	}

	// Accumulate streamed content for lastMessage tracking (Sub Agent support)
	var streamedContent strings.Builder
	var streamedReasoningContent strings.Builder

	for {
		res, timedOut := RecvWithTimeout(ctx, stream.Recv, StreamIdleTimeout)

		if timedOut {
			mgr.log(ctx, LogLevelError, "stream.idle_timeout", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
				"timeout":    StreamIdleTimeout.String(),
			})
			return &StreamIdleTimeoutError{
				SessionID: mgr.sessionID,
				TurnID:    turnID,
				Timeout:   StreamIdleTimeout,
			}
		}

		if res.Err != nil {
			return mgr.handleStreamRecvError(ctx, res.Err, turnID, agentName, role, toolName, maxUsage,
				&streamedContent, &streamedReasoningContent)
		}

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
		if err := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
			Kind:             EventKindStreamChunk,
			AgentName:        agentName,
			Role:             role,
			ToolName:         toolName,
			Content:          res.Msg.Content,
			ReasoningContent: res.Msg.ReasoningContent,
			FinishReason:     finishReason,
			TokenUsage:       tokenUsage,
		}); err != nil {
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

// mergeMaxTokenUsage merges src into dst, keeping the maximum of each field.
// Returns dst (may allocate a new TokenUsage if dst is nil and src is non-nil).
func mergeMaxTokenUsage(dst, src *schema.TokenUsage) *schema.TokenUsage {
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
