package agent

import (
	"context"

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
		h.logger.Info(ctx, "compressContextWithSessionMemory.query_error", map[string]any{
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

	h.logger.Info(ctx, "compressContextWithSessionMemory.success", map[string]any{
		"session_id":   sessionID.String(),
		"memory_count": len(memories),
	})

	return &summary, nil
}

// buildSummaryFromMemories 将 session memories 构建为摘要文本
//
// 按分类分组，格式化为可读的 markdown 格式。模板定义在
// prompts/attachments/session-memory-summary.md.tmpl。
func buildSummaryFromMemories(memories []*model.SessionMemory) string {
	return buildSummaryFromMemoriesTmpl(memories)
}

// formatSessionMemoriesForInjection 格式化 session memories 用于注入到消息中
// 用于每轮对话前的注入（与压缩时的摘要不同）。模板定义在
// prompts/attachments/session-memory-injection.md.tmpl。
func formatSessionMemoriesForInjection(memories []*model.SessionMemory) string {
	return formatSessionMemoryInjection(memories)
}
