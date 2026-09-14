package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode"

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

	// Detect user language for preamble and category labels.
	lang := a.detectUserLanguage(ctx, sessionID)

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
	categoryLabels := userMemoryLabels(lang)

	var cats []userMemoryCategory
	for _, cat := range model.ValidUserMemoryCategories {
		items := categories[cat]
		if len(items) == 0 {
			continue
		}
		cats = append(cats, userMemoryCategory{
			Label: categoryLabels[cat],
			Items: items,
		})
	}

	return formatUserMemoryWrapper(lang, cats), nil
}

// detectUserLanguage attempts to determine the user's preferred language.
//
// Strategy:
// 1. Check the session's AgentPrompt for language directives
// 2. Fall back to English (safe default)
func (a *UserMemoryAttachment) detectUserLanguage(ctx context.Context, sessionID uuid.UUID) string {
	session, err := a.helpers.deps.SessionRepo.GetByID(ctx, sessionID)
	if err != nil || session == nil {
		return "en"
	}

	// Check AgentPrompt for language directives
	prompt := strings.ToLower(session.AgentPrompt)
	if containsCJKDirective(prompt) || containsChineseHint(prompt) {
		return "zh"
	}

	return "en"
}

// containsCJKDirective checks for explicit "respond in Chinese" directives.
func containsCJKDirective(text string) bool {
	hints := []string{
		"respond in chinese",
		"respond in 中文",
		"回复使用中文",
		"用中文回复",
		"使用中文",
		"respond in zh",
	}
	for _, h := range hints {
		if strings.Contains(text, h) {
			return true
		}
	}
	return false
}

// containsChineseHint checks for Chinese-specific instructions or heavy CJK content.
func containsChineseHint(text string) bool {
	// Check for CJK-specific phrases in the prompt
	cjkHints := []string{
		"始终使用中文",
		"always respond in chinese",
		"detect the user's language",
	}
	lower := strings.ToLower(text)
	for _, h := range cjkHints {
		if strings.Contains(lower, h) {
			return true
		}
	}

	// Check if the prompt itself contains significant CJK content
	// (indicating the user configured it in Chinese)
	cjkCount := 0
	totalLetters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			totalLetters++
			if unicode.Is(unicode.Han, r) {
				cjkCount++
			}
		}
	}
	if totalLetters > 20 && float64(cjkCount)/float64(totalLetters) > 0.3 {
		return true
	}
	return false
}
