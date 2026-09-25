package agent

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// triggerSessionMemoryExtraction triggers background session memory extraction.
// This is called from data_context.go after loading messages.
func (h *helpers) triggerSessionMemoryExtraction(ctx context.Context, sessionID uuid.UUID, messages []*turnagent.Message) {
	// Early return if ChatModel is not configured (LLM disabled)
	if h.deps.ChatModel == nil {
		h.logger.Info(ctx, "[triggerSessionMemoryExtraction] skip: ChatModel is nil (LLM not configured)", map[string]any{
			"session_id": sessionID.String(),
		})
		return
	}

	// Convert turnagent.Message to schema.Message for the extractor
	schemaMessages := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		schemaMsg := &schema.Message{
			Role: schema.RoleType(msg.Role),
		}
		if msg.Content != "" {
			schemaMsg.Content = msg.Content
		}
		// Convert tool calls if present
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				schemaMsg.ToolCalls = append(schemaMsg.ToolCalls, schema.ToolCall{
					ID: tc.ID,
					Function: schema.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		schemaMessages = append(schemaMessages, schemaMsg)
	}

	// Create extractor
	extractor := NewSessionMemoryExtractor(
		h.deps.ChatModel,
		h.deps.MemoryRepo,
		turnagent.CumulativeTokenCounter,
		h.logger,
		h.noThinkingOptions(),  // disable thinking to save tokens
		h.tokenCallbackHandler, // track token consumption to Session.TotalTokens
	)

	// Load extraction state from session metadata.
	// This ensures tokenGrowth reflects the delta since the last extraction,
	// not the total token count (which would cause excessive extractions).
	state := h.loadExtractionState(ctx, sessionID)

	// Run extraction in background goroutine.
	// Use context.WithoutCancel to detach from the parent context, so extraction
	// continues even if the turn completes and the parent context is canceled.
	go func() {
		// Panic recovery: prevent a single panic from crashing the entire server.
		defer func() {
			if r := recover(); r != nil {
				bgCtx := context.Background()
				h.logger.Error(bgCtx, "[triggerSessionMemoryExtraction] panic recovered", map[string]any{
					"session_id": sessionID.String(),
					"panic":      fmt.Sprintf("%v", r),
					"stack":      string(debug.Stack()),
				})
			}
		}()

		bgCtx := context.WithoutCancel(ctx)
		// Add timeout to prevent extraction operations from hanging and causing goroutine leaks.
		bgCtx, bgTimeoutCancel := context.WithTimeout(bgCtx, 60*time.Second)
		defer bgTimeoutCancel()
		extracted, newState, err := extractor.ExtractIfNeeded(bgCtx, sessionID, schemaMessages, state)
		if err != nil {
			h.logger.Error(bgCtx, "[triggerSessionMemoryExtraction] error", map[string]any{
				"session_id": sessionID.String(),
				"error":      err.Error(),
			})
			return
		}
		if extracted && newState != nil {
			// Persist the new extraction state so the next trigger computes
			// tokenGrowth as a delta rather than using the total token count.
			h.saveExtractionState(bgCtx, sessionID, newState)
			h.logger.Info(bgCtx, "[triggerSessionMemoryExtraction] extracted", map[string]any{
				"session_id":         sessionID.String(),
				"last_token_count":   newState.LastTokenCount,
				"last_message_count": newState.LastMessageCount,
			})
		}
	}()
}

// loadExtractionState loads the persisted ExtractionState from the session record.
// Returns nil if the session cannot be loaded or no extraction has occurred yet.
func (h *helpers) loadExtractionState(ctx context.Context, sessionID uuid.UUID) *ExtractionState {
	// Use a separate timeout for this query to prevent context deadline exceeded errors
	// when the parent context is already near its timeout.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	session, err := h.deps.SessionRepo.GetByID(queryCtx, sessionID)
	if err != nil {
		h.logger.Warn(ctx, "[loadExtractionState] failed to load session", map[string]any{
			"session_id": sessionID.String(),
			"error":      err.Error(),
		})
		return nil
	}
	if session.MemoryExtractionLastTokens <= 0 && session.MemoryExtractionLastMessages <= 0 {
		return nil // Never extracted before
	}
	return &ExtractionState{
		LastTokenCount:   session.MemoryExtractionLastTokens,
		LastMessageCount: session.MemoryExtractionLastMessages,
	}
}

// saveExtractionState persists the ExtractionState to the session record.
// This is called after a successful extraction in a background goroutine.
func (h *helpers) saveExtractionState(ctx context.Context, sessionID uuid.UUID, state *ExtractionState) {
	if state == nil {
		return
	}
	err := h.deps.SessionRepo.Update(ctx, sessionID, map[string]any{
		"memory_extraction_last_tokens":   state.LastTokenCount,
		"memory_extraction_last_messages": state.LastMessageCount,
	})
	if err != nil {
		h.logger.Error(ctx, "[saveExtractionState] failed to persist state", map[string]any{
			"session_id": sessionID.String(),
			"error":      err.Error(),
		})
	}
}
