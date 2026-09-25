package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
)

// validateStructuredContent validates the content structure for feedback/project types.
// Must contain **Why:** and **How to apply:** sections.
func validateStructuredContent(content string) error {
	contentLower := strings.ToLower(content)
	hasWhy := strings.Contains(contentLower, "**why:**") || strings.Contains(contentLower, "**why**")
	hasHow := strings.Contains(contentLower, "**how to apply:**") || strings.Contains(contentLower, "**how to apply**")

	if !hasWhy && !hasHow {
		return fmt.Errorf("content must include '**Why:** ...' and '**How to apply:** ...' sections")
	}
	if !hasWhy {
		return fmt.Errorf("content must include a '**Why:** ...' section explaining the reason")
	}
	if !hasHow {
		return fmt.Errorf("content must include a '**How to apply:** ...' section explaining how to apply it")
	}
	return nil
}

// validateMemoryUpdateArgs validates the update arguments against the existing memory.
func validateMemoryUpdateArgs(args struct {
	MemoryID    string         `json:"memory_id"`
	Title       *string        `json:"title,omitempty"`
	Content     *string        `json:"content,omitempty"`
	Description *string        `json:"description,omitempty"`
	Importance  *string        `json:"importance,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}, existing *memory.Memory) error {
	if args.Importance != nil {
		validImportances := []string{"low", "medium", "high", "critical"}
		valid := false
		for _, v := range validImportances {
			if *args.Importance == v {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("invalid importance: %s", *args.Importance)
		}
	}
	if args.Content != nil &&
		(existing.Type == "feedback" || existing.Type == "project") {
		if err := validateStructuredContent(*args.Content); err != nil {
			return fmt.Errorf("content validation failed: %w", err)
		}
	}
	return nil
}

// buildMemoryUpdateFields builds the update fields map from the update arguments.
// Returns an error if metadata JSON marshaling fails.
func buildMemoryUpdateFields(args struct {
	MemoryID    string         `json:"memory_id"`
	Title       *string        `json:"title,omitempty"`
	Content     *string        `json:"content,omitempty"`
	Description *string        `json:"description,omitempty"`
	Importance  *string        `json:"importance,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}) (map[string]any, error) {
	fields := make(map[string]any)
	if args.Title != nil {
		fields["title"] = *args.Title
	}
	if args.Content != nil {
		fields["content"] = *args.Content
	}
	if args.Description != nil {
		fields["description"] = *args.Description
	}
	if args.Tags != nil {
		fields["tags"] = memory.StringArray(args.Tags)
	}
	// Handle importance: store in metadata
	if args.Importance != nil || args.Metadata != nil {
		// Merge existing metadata with new importance/metadata
		metadataMap := make(map[string]any)
		if args.Metadata != nil {
			metadataMap = args.Metadata
		}
		if args.Importance != nil {
			metadataMap["importance"] = *args.Importance
		}
		metadataJSON, err := json.Marshal(metadataMap)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata: %w", err)
		}
		fields["metadata"] = memory.JSONBString(metadataJSON)
	}
	return fields, nil
}

// persistToolError persists an error message to DB for cache consistency.
// Used by all user memory tool structs to avoid duplicating the publishToolMessages boilerplate.
func persistToolError(ctx context.Context, h *helpers, session *model.Session, turnID uuid.UUID, toolName, argumentsInJSON, errMsg string) {
	if publishErr := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         h,
		SessionID:       session.ID,
		OwnerRefID:      session.OwnerRefID,
		TurnID:          turnID,
		ToolName:        toolName,
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      errMsg,
	}); publishErr != nil {
		h.logger.Warn(ctx, toolName+".persist_error_failed", map[string]any{
			"error": publishErr.Error(),
		})
	}
}

// persistToolResult persists a success result to DB for cache consistency.
// Used by all user memory tool structs to avoid duplicating the publishToolMessages boilerplate.
func persistToolResult(ctx context.Context, h *helpers, session *model.Session, turnID uuid.UUID, toolName, argumentsInJSON, resultJSON string) {
	if err := publishToolMessages(ctx, publishToolMessagesInput{
		Helpers:         h,
		SessionID:       session.ID,
		OwnerRefID:      session.OwnerRefID,
		TurnID:          turnID,
		ToolName:        toolName,
		ArgumentsInJSON: argumentsInJSON,
		ResultJSON:      resultJSON,
	}); err != nil {
		h.logger.Warn(ctx, toolName+".publish_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// extractImportanceFromMetadata extracts the "importance" field from memory metadata JSON.
// Returns "medium" as default if not found.
func extractImportanceFromMetadata(metadata memory.JSONBString) string {
	if metadata == "" {
		return "medium"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return "medium"
	}
	v, ok := m["importance"].(string)
	if !ok || v == "" {
		return "medium"
	}
	return v
}
