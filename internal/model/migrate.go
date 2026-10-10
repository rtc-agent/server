package model

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/memory"
)

// Index Naming Convention:
//
// Indexes should follow the pattern: idx_<table>_<column1>_<column2>...
// where column names match the actual database column names (not abbreviated).
//
// Examples:
//   - idx_messages_session_offset (session_id, global_offset)
//   - idx_memory_links_from_relation (from_id, relation)
//
// Note: Some legacy indexes use abbreviated names (e.g., idx_sessions_owner_created,
// idx_admin_audit_logs_operator_time) which don't follow this convention.
// These are kept for backward compatibility but new indexes should follow the standard.

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
	if err := db.Exec("DROP INDEX IF EXISTS idx_files_user_key").Error; err != nil {
		logger.Warn(context.Background(), "migrate.drop_legacy_index_failed",
			zap.String("index", "idx_files_user_key"), zap.Error(err))
	}

	// Add composite index on memory_links to optimise queries by from_id + relation.
	// CONCURRENTLY avoids table lock during index creation.
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_memory_links_from_relation ON memory_links(from_id, relation)").Error; err != nil {
		return fmt.Errorf("create index idx_memory_links_from_relation: %w", err)
	}

	// Add unique constraint on memory_links to prevent duplicate links.
	if err := db.Exec("CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_memory_links_from_to ON memory_links(from_id, to_id)").Error; err != nil {
		return fmt.Errorf("create index uniq_memory_links_from_to: %w", err)
	}

	// Migrate user_memories.tags from text[] to jsonb (matches StringArray JSON serialization).
	// to_jsonb() converts a PostgreSQL array into a JSON array — direct cast text[]->jsonb is not allowed.
	// Check column existence and current type to avoid errors on repeated runs.
	var columnExists bool
	var isAlreadyJSONB bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'user_memories' AND column_name = 'tags'
		)
	`).Scan(&columnExists).Error; err == nil && columnExists {
		if err := db.Raw(`
			SELECT data_type = 'jsonb' FROM information_schema.columns
			WHERE table_name = 'user_memories' AND column_name = 'tags'
		`).Scan(&isAlreadyJSONB).Error; err == nil && !isAlreadyJSONB {
			// Column exists but is not jsonb, perform the conversion
			if err := db.Exec("ALTER TABLE user_memories ALTER COLUMN tags TYPE jsonb USING to_jsonb(tags)").Error; err != nil {
				logger.Warn(context.Background(), "migrate.user_memories_tags_type_change_failed",
					zap.Error(err))
			}
		}
	} else if err != nil {
		logger.Warn(context.Background(), "migrate.user_memories_tags_check_failed",
			zap.Error(err))
	}

	// Composite indexes for query performance.
	// CONCURRENTLY avoids table lock during index creation on large tables.
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_session_offset ON messages(session_id, global_offset)").Error; err != nil {
		return fmt.Errorf("create index idx_messages_session_offset: %w", err)
	}
	// Note: idx_sessions_owner (owner_kind, owner_ref_id) was merged into idx_sessions_owner_created
	// (owner_kind, owner_ref_id, created_at) in cmd/migrate.go to reduce index redundancy.
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_logs_operator_time ON admin_audit_logs(operator_id, created_at)").Error; err != nil {
		return fmt.Errorf("create index idx_admin_audit_logs_operator_time: %w", err)
	}
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_logs_resource_time ON admin_audit_logs(resource_type, created_at)").Error; err != nil {
		return fmt.Errorf("create index idx_admin_audit_logs_resource_time: %w", err)
	}

	return nil
}
