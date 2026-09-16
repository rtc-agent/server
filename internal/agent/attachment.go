// Package agent provides the Attachment system for dynamic content injection.
//
// Attachments are pieces of context that are dynamically injected into the LLM
// context on every turn (or conditionally). They provide the LLM with persistent
// state and context that goes beyond the conversation history.
//
// The Attachment system manages three types of attachments:
//   - TodoList: Current tasks and progress tracking
//   - SessionMemory: Key information extracted from the current conversation
//   - UserMemory: Long-term memories about the user and their preferences
//
// Each attachment is responsible for building its own content. The AttachmentManager
// coordinates the build process, manages token budgets, and records observability metrics.
package agent

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// Attachment is a piece of dynamic content that can be injected into LLM context.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type Attachment interface {
	// Name returns the attachment's name (used for logging and metrics).
	// Must be one of: "TodoList", "SessionMemory", "UserMemory".
	Name() string

	// Build generates the attachment content. Returns empty string if the
	// attachment should not be injected this turn.
	//
	// Build receives the sessionID and userID so it can fetch the necessary
	// data from repositories. It should be idempotent — calling it multiple
	// times with the same inputs should produce the same output.
	Build(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (string, error)
}

// AttachmentManager coordinates the building and injection of all attachments.
//
// It manages token budgets, records observability metrics, and ensures that
// attachments are built in a consistent order. The manager is created once
// per helpers instance and shared across all turns.
type AttachmentManager struct {
	attachments []Attachment
	metrics     turnagent.Metrics
	logger      turnagent.Logger

	// Token budget configuration
	maxTokensPerAttachment int // Maximum tokens for a single attachment
	totalBudget            int // Total budget for all attachments combined
}

// AttachmentManagerConfig holds configuration for the AttachmentManager.
type AttachmentManagerConfig struct {
	// MaxTokensPerAttachment is the maximum tokens allowed for a single attachment.
	// If an attachment exceeds this limit, it will be truncated.
	// If <= 0, defaults to 5000.
	MaxTokensPerAttachment int

	// TotalBudget is the total token budget for all attachments combined.
	// If the total tokens exceed this limit, lower-priority attachments
	// will be skipped.
	// If <= 0, defaults to 15000.
	TotalBudget int
}

// NewAttachmentManager creates a new AttachmentManager with the given attachments.
//
// Attachments are built in the order they are provided. If the total token count
// exceeds the budget, later attachments may be skipped.
func NewAttachmentManager(
	attachments []Attachment,
	metrics turnagent.Metrics,
	logger turnagent.Logger,
	cfg AttachmentManagerConfig,
) *AttachmentManager {
	if cfg.MaxTokensPerAttachment <= 0 {
		cfg.MaxTokensPerAttachment = 5000
	}
	if cfg.TotalBudget <= 0 {
		cfg.TotalBudget = 15000
	}

	return &AttachmentManager{
		attachments:            attachments,
		metrics:                metrics,
		logger:                 logger,
		maxTokensPerAttachment: cfg.MaxTokensPerAttachment,
		totalBudget:            cfg.TotalBudget,
	}
}

// BuildAttachments builds all attachments and returns them as system-role messages.
//
// Attachments use system role and are prepended to the message array (not appended)
// to comply with Claude API requirements. System messages must be at the start of
// the message array. Appending system-role messages after the last user message can
// cause the eino-ext Claude adapter to produce empty content blocks, making the LLM
// think the user sent nothing.
//
// The method:
// 1. Iterates through all registered attachments in order
// 2. Builds each attachment's content
// 3. Estimates token count (using 4 bytes per token)
// 4. Truncates if a single attachment exceeds MaxTokensPerAttachment
// 5. Skips remaining attachments if total budget is exceeded
// 6. Records metrics and logs for each attachment
//
// Returns a slice of system-role messages to be prepended to the conversation.
// Attachment order is preserved: the first attachment in the list appears first
// in the returned slice (highest priority position).
func (m *AttachmentManager) BuildAttachments(
	ctx context.Context,
	sessionID uuid.UUID,
	userID uuid.UUID,
) ([]*turnagent.Message, error) {
	// Collect attachment messages in order; prepend once at the end to
	// preserve the configured priority order (first attachment = highest
	// priority = first in output).
	var collected []*turnagent.Message
	var totalTokens int

	for _, att := range m.attachments {
		start := time.Now()

		// Build the attachment content
		content, err := att.Build(ctx, sessionID, userID)
		if err != nil {
			// Record failure
			duration := time.Since(start)
			m.recordMetrics(att.Name(), sessionID, 0, duration, "failed", err)
			m.logError(ctx, "attachment.build_failed", map[string]any{
				"name":       att.Name(),
				"error":      err.Error(),
				"session_id": sessionID.String(),
			})
			// Continue with next attachment (don't fail the whole turn)
			continue
		}

		// Skip empty attachments
		if content == "" {
			continue
		}

		// Estimate tokens (rough: 4 bytes per token)
		tokens := estimateStringTokens(content)

		// Check per-attachment budget
		status := "success"
		if tokens > m.maxTokensPerAttachment {
			// Capture original token count before truncation for accurate logging
			originalTokens := tokens
			// Truncate to budget
			content = truncateToTokens(content, m.maxTokensPerAttachment)
			tokens = m.maxTokensPerAttachment
			status = "truncated"

			m.logWarn(ctx, "attachment.budget_truncated", map[string]any{
				"name":             att.Name(),
				"original_tokens":  originalTokens,
				"truncated_tokens": tokens,
				"max_tokens":       m.maxTokensPerAttachment,
				"session_id":       sessionID.String(),
			})
		}

		// Check total budget
		if totalTokens+tokens > m.totalBudget {
			m.logWarn(ctx, "attachment.total_budget_exceeded", map[string]any{
				"current_total":     totalTokens,
				"next_attachment":   att.Name(),
				"attachment_tokens": tokens,
				"budget":            m.totalBudget,
				"session_id":        sessionID.String(),
			})
			// Stop injecting more attachments
			break
		}

		// Add the attachment as a system-role message.
		// Using system role and prepending (not appending) to the message array
		// prevents the Claude adapter from producing empty content blocks when
		// attachments would otherwise appear after the last user message.
		collected = append(collected, &turnagent.Message{
			Role: turnagent.RoleSystem, Content: content,
		})
		totalTokens += tokens

		// Record metrics and logs
		duration := time.Since(start)
		m.recordMetrics(att.Name(), sessionID, tokens, duration, status, nil)
		m.logDebug(ctx, "attachment.built", map[string]any{
			"name":        att.Name(),
			"tokens":      tokens,
			"duration_ms": duration.Milliseconds(),
			"session_id":  sessionID.String(),
		})
	}

	return collected, nil
}

// recordMetrics records metrics for an attachment build operation.
func (m *AttachmentManager) recordMetrics(
	name string,
	sessionID uuid.UUID,
	tokens int,
	duration time.Duration,
	status string,
	err error,
) {
	if m.metrics == nil {
		return
	}

	m.metrics.RecordAttachment(context.Background(), turnagent.AttachmentMetricsAttrs{
		Name:       name,
		SessionID:  sessionID.String(),
		Tokens:     tokens,
		DurationMs: duration.Milliseconds(),
		Status:     status,
		Error:      err,
	})
}

// logDebug logs a debug message.
func (m *AttachmentManager) logDebug(ctx context.Context, msg string, fields map[string]any) {
	m.logger.Debug(ctx, msg, fields)
}

// logWarn logs a warning message.
func (m *AttachmentManager) logWarn(ctx context.Context, msg string, fields map[string]any) {
	m.logger.Warn(ctx, msg, fields)
}

// logError logs an error message.
func (m *AttachmentManager) logError(ctx context.Context, msg string, fields map[string]any) {
	m.logger.Error(ctx, msg, fields)
}

// estimateStringTokens estimates the number of tokens in a string.
// Uses the global TokenCounter (configurable: heuristic or tiktoken).
// Falls back to 4 bytes per token with 1.33x safety margin for backward compatibility.
func estimateStringTokens(s string) int {
	tokens := turnagent.CountStringTokens(s)
	// Apply 1.33x safety margin for backward compatibility
	return int(float64(tokens) * 1.33)
}

// truncateToTokens truncates a string to fit within the given token budget.
// Keeps the head (60%) and tail (20%) of the content, with a truncation marker in the middle.
// Respects UTF-8 character boundaries to avoid splitting multi-byte characters.
func truncateToTokens(s string, maxTokens int) string {
	maxChars := int(float64(maxTokens) * 4.0 / 1.33) // Reverse the token estimation
	if len(s) <= maxChars {
		return s
	}

	// Keep head 60% + tail 20%, respecting UTF-8 boundaries.
	headChars := int(float64(maxChars) * 0.6)
	tailChars := int(float64(maxChars) * 0.2)

	// Walk backward from headChars to find a valid UTF-8 rune boundary.
	for headChars > 0 && !utf8.RuneStart(s[headChars]) {
		headChars--
	}
	head := s[:headChars]

	// For the tail, walk backward from the end to find a valid UTF-8 rune boundary.
	tailStart := len(s) - tailChars
	for tailStart > 0 && !utf8.RuneStart(s[tailStart]) {
		tailStart--
	}
	tail := s[tailStart:]

	return head + "\n\n[Content truncated - exceeded token limit]\n\n" + tail
}
