package model

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/memory"
)

// 索引命名规范:
//
// 索引应遵循模式: idx_<表名>_<列名1>_<列名2>...
// 其中列名应与实际数据库列名一致（不缩写）。
//
// 示例:
//   - idx_messages_session_offset (session_id, global_offset)
//   - idx_memory_links_from_relation (from_id, relation)
//
// 注意: 部分遗留索引使用缩写名称（如 idx_sessions_owner_created,
// idx_admin_audit_logs_operator_time），不符合此规范。
// 为保持向后兼容，这些索引保留不变，但新索引应遵循标准命名规范。

// SchemaMigration tracks applied database migrations.
// This enables version control, debugging, and rollback capabilities.
type SchemaMigration struct {
	Version    int64     `gorm:"primaryKey;autoIncrement:false"`
	AppliedAt  time.Time `gorm:"not null;default:now()"`
	DurationMs int64     `gorm:"not null"`
	Status     string    `gorm:"not null;size:32"` // "completed", "failed", "partial"
}

// TableName specifies the table name for SchemaMigration.
func (SchemaMigration) TableName() string {
	return "schema_migrations"
}

// migrationVersion is the current schema migration version in YYYYMMDDHHMMSS format.
// Used to track which migrations have been applied.
const migrationVersion = 20261013000001

// recordMigration records a migration completion or failure.
func recordMigration(db *gorm.DB, duration time.Duration, status string) error {
	migration := SchemaMigration{
		Version:    migrationVersion,
		AppliedAt:  time.Now(),
		DurationMs: duration.Milliseconds(),
		Status:     status,
	}
	return db.Create(&migration).Error
}

// ensureSchemaMigrationsTable creates the schema_migrations table if it doesn't exist.
func ensureSchemaMigrationsTable(db *gorm.DB) error {
	return db.AutoMigrate(&SchemaMigration{})
}

// TODO(migration): Make batch backfill sleep duration configurable.
// Current state: Large table migrations (e.g., memory_links index creation) may need
// batch processing with sleep intervals to avoid overwhelming the database. However,
// the sleep duration is not configurable and may not suit all deployment scenarios.
//
// Consider:
//   - Read sleep duration from environment variable or config file
//   - Add batch size configuration for large operations
//   - Implement adaptive sleep based on database load metrics
//   - Example: MIGRATION_BATCH_SLEEP_MS=100, MIGRATION_BATCH_SIZE=1000

// TODO(migration): Add migration progress monitoring and reporting.
// Current state: Migrations run silently with no progress indication for long-running
// operations. This makes it difficult to:
//   - Estimate completion time for large migrations
//   - Detect stalled or slow migrations
//   - Provide feedback to operators during deployment
//
// Proposed enhancements:
//   - Log progress for operations on large tables (e.g., "processed 1000/10000 rows")
//   - Emit Prometheus metrics for migration progress
//   - Add timeout per migration with automatic rollback
//   - Implement dry-run mode to estimate migration duration
//   - Example progress log:
//     logger.Info("migration progress",
//       zap.String("migration", "backfill_memory_links"),
//       zap.Int("processed", 1000),
//       zap.Int("total", 10000),
//       zap.Duration("elapsed", 5*time.Second))

// AutoMigrate runs GORM auto-migration for all models.
func AutoMigrate(db *gorm.DB) error {
	// Ensure schema_migrations table exists for version tracking.
	startTime := time.Now()
	if err := ensureSchemaMigrationsTable(db); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	// Check if this migration version has already been applied.
	const migrationVersion = 20261013000001 // YYYYMMDDHHMMSS format
	var alreadyApplied bool
	if err := db.Raw("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ? AND status = 'completed')", migrationVersion).Scan(&alreadyApplied).Error; err != nil {
		logger.Warn(context.Background(), "migrate.check_version_failed", zap.Error(err))
		// Continue anyway - idempotent operations will handle this safely
	}

	if alreadyApplied {
		logger.Info(context.Background(), "migrate.already_applied",
			zap.Int64("version", migrationVersion))
	}

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
		// Record failed migration
		_ = recordMigration(db, time.Since(startTime), "failed")
		return err
	}

	// Drop legacy composite unique index on files (user_id, key) if it exists.
	// The unique constraint is now on (key) only — content-addressed storage
	// guarantees key uniqueness per content hash. See oss3-phase3 review Task 4.
	// Using CONCURRENTLY to avoid blocking table operations during index drop.
	// Note: DROP INDEX CONCURRENTLY cannot run inside a transaction block.
	if err := db.Exec("DROP INDEX CONCURRENTLY IF EXISTS idx_files_user_key").Error; err != nil {
		logger.Warn(context.Background(), "migrate.drop_legacy_index_failed",
			zap.String("index", "idx_files_user_key"), zap.Error(err))
	}

	// Fix 8: Drop redundant idx_sessions_owner index (replaced by idx_sessions_owner_created).
	// This reduces index maintenance overhead without impacting query performance.
	if err := db.Exec("DROP INDEX CONCURRENTLY IF EXISTS idx_sessions_owner").Error; err != nil {
		logger.Warn(context.Background(), "migrate.drop_redundant_index_failed",
			zap.String("index", "idx_sessions_owner"), zap.Error(err))
	}

	// Add composite index on memory_links to optimise queries by from_id + relation.
	// CONCURRENTLY avoids table lock during index creation.
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_memory_links_from_relation ON memory_links(from_id, relation)").Error; err != nil {
		_ = recordMigration(db, time.Since(startTime), "failed")
		return fmt.Errorf("create index idx_memory_links_from_relation: %w", err)
	}

	// Add unique constraint on memory_links to prevent duplicate links.
	if err := db.Exec("CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_memory_links_from_to ON memory_links(from_id, to_id)").Error; err != nil {
		_ = recordMigration(db, time.Since(startTime), "failed")
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
		_ = recordMigration(db, time.Since(startTime), "failed")
		return fmt.Errorf("create index idx_messages_session_offset: %w", err)
	}
	// Note: idx_sessions_owner (owner_kind, owner_ref_id) was merged into idx_sessions_owner_created
	// (owner_kind, owner_ref_id, created_at) in cmd/migrate.go to reduce index redundancy.
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_logs_operator_time ON admin_audit_logs(operator_id, created_at)").Error; err != nil {
		_ = recordMigration(db, time.Since(startTime), "failed")
		return fmt.Errorf("create index idx_admin_audit_logs_operator_time: %w", err)
	}
	if err := db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_admin_audit_logs_resource_time ON admin_audit_logs(resource_type, created_at)").Error; err != nil {
		_ = recordMigration(db, time.Since(startTime), "failed")
		return fmt.Errorf("create index idx_admin_audit_logs_resource_time: %w", err)
	}

	// Record successful migration completion
	if !alreadyApplied {
		if err := recordMigration(db, time.Since(startTime), "completed"); err != nil {
			logger.Warn(context.Background(), "migrate.record_migration_failed",
				zap.Int64("version", migrationVersion), zap.Error(err))
		}
	}

	return nil
}
