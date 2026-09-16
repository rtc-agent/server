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
	updateMaxUsage := func(usage *schema.TokenUsage) {
		if usage == nil {
			return
		}
		if maxUsage == nil {
			maxUsage = &schema.TokenUsage{}
		}
		if usage.PromptTokens > maxUsage.PromptTokens {
			maxUsage.PromptTokens = usage.PromptTokens
		}
		if usage.CompletionTokens > maxUsage.CompletionTokens {
			maxUsage.CompletionTokens = usage.CompletionTokens
		}
		if usage.TotalTokens > maxUsage.TotalTokens {
			maxUsage.TotalTokens = usage.TotalTokens
		}
		if usage.PromptTokenDetails.CachedTokens > maxUsage.PromptTokenDetails.CachedTokens {
			maxUsage.PromptTokenDetails.CachedTokens = usage.PromptTokenDetails.CachedTokens
		}
		if usage.CompletionTokensDetails.ReasoningTokens > maxUsage.CompletionTokensDetails.ReasoningTokens {
			maxUsage.CompletionTokensDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
		}
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
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				mgr.log(ctx, LogLevelInfo, "stream.ctx_cancelled", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
				})
				return res.Err
			}
			if errors.Is(res.Err, io.EOF) {
				// For assistant messages, set lastMessage from accumulated content
				// This is needed for Sub Agent support to report the final result
				if role == string(schema.Assistant) {
					content := streamedContent.String()
					reasoning := streamedReasoningContent.String()
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
			if errors.As(res.Err, &cancelErr) {
				return nil
			}
			if pubErr := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
				Kind:      EventKindError,
				AgentName: agentName,
				Err:       res.Err,
			}); pubErr != nil {
				return pubErr
			}
			return res.Err
		}

		var finishReason string
		var tokenUsage *TokenUsage
		if res.Msg.ResponseMeta != nil {
			finishReason = res.Msg.ResponseMeta.FinishReason
			updateMaxUsage(res.Msg.ResponseMeta.Usage)
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
