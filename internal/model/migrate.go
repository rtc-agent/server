package model

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/memory"
)

// AutoMigrate runs GORM auto-migration for all models.
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&OAuth2User{},
		&Device{},
		&Session{},
		&Turn{},
		&Message{},
		&Rtc{},
		&RefreshToken{},
		&UserUpdate{},
		&SessionMemory{},
		&UserMemory{},
		&Goal{},
		&Loop{},
		&ScriptExecution{},
		// Phase 2: unified Memory model
		&memory.Memory{},
		&memory.MemoryLink{},
		// rtc-oss3: object storage models
		&File{},
		&MultipartUpload{},
		&MultipartUploadPart{},
		&TemporaryCredential{},
	); err != nil {
		return err
	}

	// Drop legacy composite unique index on files (user_id, key) if it exists.
	// The unique constraint is now on (key) only — content-addressed storage
	// guarantees key uniqueness per content hash. See oss3-phase3 review Task 4.
	_ = db.Exec("DROP INDEX IF EXISTS idx_files_user_key").Error

	// Add composite index on memory_links to optimise queries by from_id + relation.
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_memory_links_from_relation ON memory_links(from_id, relation)").Error; err != nil {
		return fmt.Errorf("create index idx_memory_links_from_relation: %w", err)
	}

	// Add unique constraint on memory_links to prevent duplicate links.
	if err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS uniq_memory_links_from_to ON memory_links(from_id, to_id)").Error; err != nil {
		return fmt.Errorf("create index uniq_memory_links_from_to: %w", err)
	}

	// Migrate user_memories.tags from text[] to jsonb (matches StringArray JSON serialization).
	// to_jsonb() converts a PostgreSQL array into a JSON array — direct cast text[]->jsonb is not allowed.
	// Silently ignored if the column is already jsonb or doesn't exist.
	_ = db.Exec("ALTER TABLE user_memories ALTER COLUMN tags TYPE jsonb USING to_jsonb(tags)").Error

	return nil
}
