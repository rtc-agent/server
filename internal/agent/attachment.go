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

// BuildAttachments builds all attachments and returns them as system messages.
//
// The method:
// 1. Iterates through all registered attachments in order
// 2. Builds each attachment's content
// 3. Estimates token count (using 4 bytes per token)
// 4. Truncates if a single attachment exceeds MaxTokensPerAttachment
// 5. Skips remaining attachments if total budget is exceeded
// 6. Records metrics and logs for each attachment
//
// Returns a slice of system messages ready to be appended to the conversation.
func (m *AttachmentManager) BuildAttachments(
	ctx context.Context,
	sessionID uuid.UUID,
	userID uuid.UUID,
) ([]*turnagent.Message, error) {
	var messages []*turnagent.Message
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
			// Truncate to budget
			content = truncateToTokens(content, m.maxTokensPerAttachment)
			tokens = m.maxTokensPerAttachment
			status = "truncated"

			m.logWarn(ctx, "attachment.budget_truncated", map[string]any{
				"name":              att.Name(),
				"original_tokens":   estimateStringTokens(content),
				"truncated_tokens":  tokens,
				"max_tokens":        m.maxTokensPerAttachment,
				"session_id":        sessionID.String(),
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

		// Add the attachment as a system message
		messages = append(messages, &turnagent.Message{
			Role:    turnagent.RoleSystem,
			Content: content,
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

	return messages, nil
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

// logDebug logs a debug message if the logger is available.
func (m *AttachmentManager) logDebug(ctx context.Context, msg string, fields map[string]any) {
	if m.logger != nil {
		m.logger.Debug(ctx, msg, fields)
	}
}

// logWarn logs a warning message if the logger is available.
func (m *AttachmentManager) logWarn(ctx context.Context, msg string, fields map[string]any) {
	if m.logger != nil {
		m.logger.Warn(ctx, msg, fields)
	}
}

// logError logs an error message if the logger is available.
func (m *AttachmentManager) logError(ctx context.Context, msg string, fields map[string]any) {
	if m.logger != nil {
		m.logger.Error(ctx, msg, fields)
	}
}

// estimateStringTokens estimates the number of tokens in a string.
// Uses a rough heuristic: 4 bytes per token, with a 1.33x safety margin.
func estimateStringTokens(s string) int {
	return int(float64(len(s)) / 4.0 * 1.33)
}

// truncateToTokens truncates a string to fit within the given token budget.
// Keeps the head (60%) and tail (20%) of the content, with a truncation marker in the middle.
func truncateToTokens(s string, maxTokens int) string {
	maxChars := int(float64(maxTokens) * 4.0 / 1.33) // Reverse the token estimation
	if len(s) <= maxChars {
		return s
	}

	// Keep head 60% + tail 20%
	headChars := int(float64(maxChars) * 0.6)
	tailChars := int(float64(maxChars) * 0.2)

	head := s[:headChars]
	tail := s[len(s)-tailChars:]

	return head + "\n\n[Content truncated - exceeded token limit]\n\n" + tail
}
