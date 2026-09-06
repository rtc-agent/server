package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// UserMemoryAttachment injects user memories into LLM context.
//
// User memories are long-term memories about the user, their preferences,
// projects, and feedback. They persist across sessions and provide the LLM
// with context about the user.
//
// This attachment injects memories filtered by importance:
// - critical and high importance: always injected
// - medium importance: injected if count per category < 10
// - low importance: never injected
//
// Memories are grouped by category and formatted for easy consumption.
type UserMemoryAttachment struct {
	helpers *helpers
}

// NewUserMemoryAttachment creates a new UserMemoryAttachment.
func NewUserMemoryAttachment(h *helpers) *UserMemoryAttachment {
	return &UserMemoryAttachment{helpers: h}
}

// Name returns the attachment name.
func (a *UserMemoryAttachment) Name() string {
	return "UserMemory"
}

// Build generates the user memory content for injection.
//
// Returns empty string if:
// - UserMemoryRepo is nil (feature disabled)
// - No memories exist for the user
// - Query fails
//
// The output is wrapped in <user_memory> tags and grouped by category:
// - 关于用户 (user)
// - 工作偏好与反馈 (feedback)
// - 项目信息 (project)
// - 参考资料 (reference)
func (a *UserMemoryAttachment) Build(ctx context.Context, sessionID uuid.UUID, userID uuid.UUID) (string, error) {
	if a.helpers.deps.UserMemoryRepo == nil {
		return "", nil
	}

	// Get all user memories (limited, sorted by importance)
	memories, err := a.helpers.deps.UserMemoryRepo.ListByUser(ctx, userID, 50)
	if err != nil {
		return "", fmt.Errorf("query user memories: %w", err)
	}

	if len(memories) == 0 {
		return "", nil
	}

	// Split by importance: critical/high first, then medium
	var criticalHigh []*model.UserMemory
	var medium []*model.UserMemory

	for _, mem := range memories {
		switch mem.Importance {
		case model.ImportanceCritical, model.ImportanceHigh:
			criticalHigh = append(criticalHigh, mem)
		case model.ImportanceMedium:
			medium = append(medium, mem)
		// low importance: skip
		}
	}

	// Build the output
	var sb strings.Builder
	sb.WriteString("<user_memory>\n")
	sb.WriteString("以下是关于当前用户的持久记忆。请在回复时参考这些信息，避免重复询问已知内容。\n\n")

	// Group by category
	categories := map[string][]*model.UserMemory{
		model.UserMemoryCategoryUser:      {},
		model.UserMemoryCategoryFeedback:  {},
		model.UserMemoryCategoryProject:   {},
		model.UserMemoryCategoryReference: {},
	}

	// Add critical/high first
	for _, mem := range criticalHigh {
		categories[mem.Category] = append(categories[mem.Category], mem)
	}
	// Then add medium (up to 10 per category)
	for _, mem := range medium {
		if len(categories[mem.Category]) < 10 {
			categories[mem.Category] = append(categories[mem.Category], mem)
		}
	}

	// Output by category
	categoryLabels := map[string]string{
		model.UserMemoryCategoryUser:      "关于用户",
		model.UserMemoryCategoryFeedback:  "工作偏好与反馈",
		model.UserMemoryCategoryProject:   "项目信息",
		model.UserMemoryCategoryReference: "参考资料",
	}

	for _, cat := range model.ValidUserMemoryCategories {
		items := categories[cat]
		if len(items) == 0 {
			continue
		}
		label := categoryLabels[cat]
		fmt.Fprintf(&sb, "## %s\n", label)
		for _, mem := range items {
			sb.WriteString(formatUserMemoryForInjection(mem))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("</user_memory>")

	return sb.String(), nil
}

// formatUserMemoryForInjection formats a single user memory for injection into LLM context.
func formatUserMemoryForInjection(mem *model.UserMemory) string {
	var sb strings.Builder

	// Frontmatter with metadata
	sb.WriteString("### ")
	sb.WriteString(mem.Title)
	sb.WriteString("\n")

	// Content
	sb.WriteString(mem.Content)
	sb.WriteString("\n\n")

	return sb.String()
}
