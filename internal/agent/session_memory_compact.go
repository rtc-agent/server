package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
)

// compressContextWithSessionMemory 尝试使用 session memory 进行压缩
// 如果没有 session memory，返回 nil 让调用方回退到 LLM 生成摘要
//
// Strategy:
//  1. Query session memories for the current session
//  2. If memories exist, build summary from them (zero API cost)
//  3. Otherwise, return nil to fall back to LLM summarization
func (h *helpers) compressContextWithSessionMemory(
	ctx context.Context,
	msgs []*schema.Message,
	retentionIndex int,
) (*string, error) {
	sessionID := getSessionIDFromContext(ctx)
	if sessionID == uuid.Nil {
		return nil, nil // No session ID, fall back to LLM
	}

	// Query session memories
	memories, err := h.deps.SessionMemoryRepo.ListBySession(ctx, sessionID, 20)
	if err != nil {
		h.logIfEnabled(ctx, "compressContextWithSessionMemory.query_error", map[string]any{
			"session_id": sessionID.String(),
			"error":      err.Error(),
		})
		return nil, nil // Fall back to LLM on error
	}

	if len(memories) == 0 {
		return nil, nil // No memories, fall back to LLM
	}

	// Build summary from memories
	summary := buildSummaryFromMemories(memories)

	h.logIfEnabled(ctx, "compressContextWithSessionMemory.success", map[string]any{
		"session_id":  sessionID.String(),
		"memory_count": len(memories),
	})

	return &summary, nil
}

// buildSummaryFromMemories 将 session memories 构建为摘要文本
//
// 按分类分组，格式化为可读的 markdown 格式
func buildSummaryFromMemories(memories []*model.SessionMemory) string {
	if len(memories) == 0 {
		return ""
	}

	// 按分类分组
	grouped := make(map[string][]*model.SessionMemory)
	for _, mem := range memories {
		grouped[mem.Category] = append(grouped[mem.Category], mem)
	}

	// 按 created_at 排序（最新的在前）
	for _, items := range grouped {
		sort.Slice(items, func(i, j int) bool {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		})
	}

	var sb strings.Builder

	sb.WriteString("# Session Memory\n\n")
	sb.WriteString("This is a summary of the current session, extracted from the conversation history.\n\n")

	// 定义分类顺序和显示名称
	categories := []struct {
		key         string
		displayName string
	}{
		{model.SessionMemoryCategoryContext, "Current Context"},
		{model.SessionMemoryCategoryProgress, "Progress"},
		{model.SessionMemoryCategoryDecision, "Decisions"},
		{model.SessionMemoryCategoryIssue, "Issues & Solutions"},
		{model.SessionMemoryCategoryLearnings, "Learnings"},
	}

	for _, cat := range categories {
		items, ok := grouped[cat.key]
		if !ok || len(items) == 0 {
			continue
		}

		fmt.Fprintf(&sb, "## %s\n\n", cat.displayName)
		for _, item := range items {
			fmt.Fprintf(&sb, "### %s\n\n", item.Title)
			sb.WriteString(item.Content)
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// formatSessionMemoriesForInjection 格式化 session memories 用于注入到消息中
// 用于每轮对话前的注入（与压缩时的摘要不同）
func formatSessionMemoriesForInjection(memories []*model.SessionMemory) string {
	if len(memories) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("<system-reminder>\n")
	sb.WriteString("## Session Memory (Current Session)\n\n")
	sb.WriteString("Key information from this session:\n\n")

	// 按分类分组，简化显示
	grouped := make(map[string][]*model.SessionMemory)
	for _, mem := range memories {
		grouped[mem.Category] = append(grouped[mem.Category], mem)
	}

	// 简化显示：每个分类最多显示 3 条
	categories := []string{
		model.SessionMemoryCategoryContext,
		model.SessionMemoryCategoryProgress,
		model.SessionMemoryCategoryDecision,
		model.SessionMemoryCategoryIssue,
		model.SessionMemoryCategoryLearnings,
	}

	for _, cat := range categories {
		items, ok := grouped[cat]
		if !ok || len(items) == 0 {
			continue
		}

		// 只显示最新的 3 条
		limit := 3
		if len(items) < limit {
			limit = len(items)
		}

		for i := 0; i < limit; i++ {
			item := items[i]
			// 截断过长的内容
			content := item.Content
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			fmt.Fprintf(&sb, "- **[%s]** %s: %s\n", cat, item.Title, content)
		}
	}

	sb.WriteString("</system-reminder>")

	return sb.String()
}
