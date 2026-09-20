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

// MigrateToUnifiedMemory migrates data from the legacy tables (session_memories
// and user_memories) into the unified memories table. This function is
// idempotent — it should run on an empty new table, or only after confirming
// there are no duplicates. The legacy tables are not dropped, allowing rollback.
func MigrateToUnifiedMemory(ctx context.Context, db *gorm.DB) error {
	// Migrate session_memories.
	if err := migrateSessionMemories(ctx, db); err != nil {
		return fmt.Errorf("migrate session_memories: %w", err)
	}

	// Migrate user_memories.
	if err := migrateUserMemories(ctx, db); err != nil {
		return fmt.Errorf("migrate user_memories: %w", err)
	}

	return nil
}

// ─── SessionMemory migration ───

func migrateSessionMemories(ctx context.Context, db *gorm.DB) error {
	var oldMemories []model.SessionMemory
	if err := db.WithContext(ctx).Unscoped().Find(&oldMemories).Error; err != nil {
		// Skip if the table does not exist (first deployment may have no legacy data).
		return nil
	}

	for _, old := range oldMemories {
		// Check if already migrated (idempotency guard).
		var existing memory.Memory
		err := db.WithContext(ctx).Unscoped().First(&existing, "id = ?", old.ID).Error
		if err == nil {
			continue // Already exists, skip.
		}
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("check existing memory %s: %w", old.ID, err)
		}

		// Convert Metadata: JSONObject (JSON object) -> JSONBString (JSON object or raw).
		var metadataStr memory.JSONBString
		if len(old.Metadata) > 0 {
			b, err := json.Marshal(map[string]any(old.Metadata))
			if err == nil {
				metadataStr = memory.JSONBString(b)
			}
		}

		// TokenCount: *int -> int.
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
			Timestamp:  old.CreatedAt, // no separate timestamp field, use CreatedAt
			CreatedAt:  old.CreatedAt,
			UpdatedAt:  old.UpdatedAt,
		}

		// DeletedAt: *time.Time -> gorm.DeletedAt.
		if old.DeletedAt != nil {
			newMem.DeletedAt = gorm.DeletedAt{Time: *old.DeletedAt, Valid: true}
		}

		if err := db.WithContext(ctx).Create(newMem).Error; err != nil {
			return fmt.Errorf("migrate session memory %s: %w", old.ID, err)
		}
	}

	return nil
}

// ─── UserMemory migration ───

func migrateUserMemories(ctx context.Context, db *gorm.DB) error {
	var oldMemories []model.UserMemory
	if err := db.WithContext(ctx).Unscoped().Find(&oldMemories).Error; err != nil {
		// Skip if the table does not exist.
		return nil
	}

	for _, old := range oldMemories {
		// Check if already migrated (idempotency guard).
		var existing memory.Memory
		err := db.WithContext(ctx).Unscoped().First(&existing, "id = ?", old.ID).Error
		if err == nil {
			continue // Already exists, skip.
		}
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("check existing memory %s: %w", old.ID, err)
		}

		// Tags: model.StringArray (JSONB[string]) -> memory.StringArray (JSONB []string).
		// Both are JSON arrays under the hood, so convert directly.
		tags := memory.StringArray(old.Tags)

		// Build OKF-compatible Metadata.
		metadataStr := buildUserMemoryMetadata(old)

		// TokenCount: estimate.
		tokenCount := estimateTokens(old.Content)

		// Description: *string -> string.
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

		// DeletedAt: *time.Time -> gorm.DeletedAt.
		if old.DeletedAt != nil {
			newMem.DeletedAt = gorm.DeletedAt{Time: *old.DeletedAt, Valid: true}
		}

		if err := db.WithContext(ctx).Create(newMem).Error; err != nil {
			return fmt.Errorf("migrate user memory %s: %w", old.ID, err)
		}
	}

	return nil
}

// ─── Helper functions ───

// buildUserMemoryMetadata converts the extended fields of UserMemory into
// OKF-compatible Metadata JSONB.
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

// estimateTokens estimates the token count for a text string.
// Approximation: non-ASCII characters (e.g., Chinese) are ~1-2 tokens/rune,
// ASCII characters are ~0.25 tokens/rune. Simple strategy: non-ASCII = 2
// tokens/rune, ASCII = 1 token/rune, then halve for the average.
func estimateTokens(content string) int {
	if len(content) == 0 {
		return 0
	}
	count := 0
	for _, r := range content {
		if r > 127 {
			count += 2 // non-ASCII (Chinese, etc.) ~1-2 tokens
		} else {
			count += 1 // ASCII
		}
	}
	return count / 2 // average estimate
}
