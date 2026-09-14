package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
)

// MigrateToUnifiedMemory 将旧表 (session_memories + user_memories) 数据迁移到统一 memories 表。
// 该函数是幂等的——应在空的新表上运行，或仅在确认无重复后调用。
// 旧表不会被删除，以便回滚。
func MigrateToUnifiedMemory(ctx context.Context, db *gorm.DB) error {
	// 迁移 session_memories
	if err := migrateSessionMemories(ctx, db); err != nil {
		return fmt.Errorf("migrate session_memories: %w", err)
	}

	// 迁移 user_memories
	if err := migrateUserMemories(ctx, db); err != nil {
		return fmt.Errorf("migrate user_memories: %w", err)
	}

	return nil
}

// ─── SessionMemory 迁移 ───

func migrateSessionMemories(ctx context.Context, db *gorm.DB) error {
	var oldMemories []model.SessionMemory
	if err := db.WithContext(ctx).Unscoped().Find(&oldMemories).Error; err != nil {
		// 表不存在时跳过（首次部署可能没有旧数据）
		return nil
	}

	for _, old := range oldMemories {
		// 检查是否已迁移（幂等性保护）
		var existing memory.Memory
		err := db.WithContext(ctx).Unscoped().First(&existing, "id = ?", old.ID).Error
		if err == nil {
			continue // 已存在，跳过
		}
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("check existing memory %s: %w", old.ID, err)
		}

		// 转换 Metadata: JSONB[any] (JSON array) -> JSONBString (JSON object or raw)
		var metadataStr memory.JSONBString
		if len(old.Metadata) > 0 {
			b, err := json.Marshal([]any(old.Metadata))
			if err == nil {
				metadataStr = memory.JSONBString(b)
			}
		}

		// TokenCount: *int -> int
		tokenCount := 0
		if old.TokenCount != nil {
			tokenCount = *old.TokenCount
		}

		newMem := &memory.Memory{
			ID:         old.ID,
			Scope:      memory.ScopeSession,
			ScopeID:    old.SessionID,
			Type:       old.Category, // categories already align with OKF types
			Title:      old.Title,
			Content:    old.Content,
			Metadata:   metadataStr,
			TokenCount: tokenCount,
			Timestamp:  old.CreatedAt, // 无独立 timestamp，使用 CreatedAt
			CreatedAt:  old.CreatedAt,
			UpdatedAt:  old.UpdatedAt,
		}

		// DeletedAt: *time.Time -> gorm.DeletedAt
		if old.DeletedAt != nil {
			newMem.DeletedAt = gorm.DeletedAt{Time: *old.DeletedAt, Valid: true}
		}

		if err := db.WithContext(ctx).Create(newMem).Error; err != nil {
			return fmt.Errorf("migrate session memory %s: %w", old.ID, err)
		}
	}

	return nil
}

// ─── UserMemory 迁移 ───

func migrateUserMemories(ctx context.Context, db *gorm.DB) error {
	var oldMemories []model.UserMemory
	if err := db.WithContext(ctx).Unscoped().Find(&oldMemories).Error; err != nil {
		// 表不存在时跳过
		return nil
	}

	for _, old := range oldMemories {
		// 检查是否已迁移（幂等性保护）
		var existing memory.Memory
		err := db.WithContext(ctx).Unscoped().First(&existing, "id = ?", old.ID).Error
		if err == nil {
			continue // 已存在，跳过
		}
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("check existing memory %s: %w", old.ID, err)
		}

		// Tags: model.StringArray (JSONB[string]) -> memory.StringArray (JSONB []string)
		// 底层都是 JSON 数组，直接转换
		tags := memory.StringArray(old.Tags)

		// 构建 OKF 兼容的 Metadata
		metadataStr := buildUserMemoryMetadata(old)

		// TokenCount: 估算
		tokenCount := estimateTokens(old.Content)

		// Description: *string -> string
		description := ""
		if old.Description != nil {
			description = *old.Description
		}

		newMem := &memory.Memory{
			ID:          old.ID,
			Scope:       memory.ScopeUser,
			ScopeID:     old.UserID,
			Type:        old.Category, // categories already align with OKF types
			Title:       old.Title,
			Description: description,
			Content:     old.Content,
			Tags:        tags,
			Metadata:    metadataStr,
			TokenCount:  tokenCount,
			Timestamp:   old.CreatedAt,
			CreatedAt:   old.CreatedAt,
			UpdatedAt:   old.UpdatedAt,
		}

		// DeletedAt: *time.Time -> gorm.DeletedAt
		if old.DeletedAt != nil {
			newMem.DeletedAt = gorm.DeletedAt{Time: *old.DeletedAt, Valid: true}
		}

		if err := db.WithContext(ctx).Create(newMem).Error; err != nil {
			return fmt.Errorf("migrate user memory %s: %w", old.ID, err)
		}
	}

	return nil
}

// ─── 辅助函数 ───

// buildUserMemoryMetadata 将 UserMemory 的扩展字段构建为 OKF Metadata JSONB
func buildUserMemoryMetadata(old model.UserMemory) memory.JSONBString {
	metadata := map[string]any{
		"importance":   old.Importance,
		"access_count": old.AccessCount,
	}

	if old.SourceSessionID != nil {
		metadata["sources"] = []map[string]any{
			{
				"id":       fmt.Sprintf("session-%s", old.SourceSessionID.String()[:8]),
				"resource": fmt.Sprintf("rtc-agent://session/%s", old.SourceSessionID.String()),
			},
		}
	}

	if old.LastAccessedAt != nil {
		metadata["last_accessed_at"] = old.LastAccessedAt.Format(time.RFC3339)
	}

	b, _ := json.Marshal(metadata)
	return memory.JSONBString(b)
}

// estimateTokens 估算文本的 token 数。
// 按 rune 计算：非 ASCII 字符（如中文）约 1-2 token/rune，ASCII 字符约 0.25 token/rune。
// 简单策略：非 ASCII 按 1 token/rune，ASCII 按 0.5 token/4chars，取平均值。
func estimateTokens(content string) int {
	if len(content) == 0 {
		return 0
	}
	count := 0
	for _, r := range content {
		if r > 127 {
			count += 2 // 非 ASCII（中文等）约 1-2 token
		} else {
			count += 1 // ASCII
		}
	}
	return count / 2 // 平均估算
}
