package agent

import (
	"fmt"
	"strings"

	"github.com/rtc-agent/server/internal/model"
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
}, existing *model.UserMemory) error {
	if args.Importance != nil && !model.IsValidImportance(*args.Importance) {
		return fmt.Errorf("invalid importance: %s", *args.Importance)
	}
	if args.Content != nil &&
		(existing.Category == model.UserMemoryCategoryFeedback || existing.Category == model.UserMemoryCategoryProject) {
		if err := validateStructuredContent(*args.Content); err != nil {
			return fmt.Errorf("content validation failed: %w", err)
		}
	}
	return nil
}

// buildMemoryUpdateFields builds the update fields map from the update arguments.
func buildMemoryUpdateFields(args struct {
	MemoryID    string         `json:"memory_id"`
	Title       *string        `json:"title,omitempty"`
	Content     *string        `json:"content,omitempty"`
	Description *string        `json:"description,omitempty"`
	Importance  *string        `json:"importance,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}) map[string]any {
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
	if args.Importance != nil {
		fields["importance"] = *args.Importance
	}
	if args.Tags != nil {
		fields["tags"] = model.StringArray(args.Tags)
	}
	if args.Metadata != nil {
		fields["metadata"] = model.JSONObject(args.Metadata)
	}
	return fields
}
