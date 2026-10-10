package model

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/logger"
)

// MigrateOwnerStages1And2 adds new columns and backfills from legacy user_id.
// Called by cmd/migrate.go in T2.
func MigrateOwnerStages1And2(db *gorm.DB) error {
	if err := addOwnerAndCreatorColumns(db); err != nil {
		return err
	}
	return backfillOwnerAndCreator(db)
}

// MigrateOwnerStage3 sets NOT NULL constraints. Called in T3 after all code uses new fields.
func MigrateOwnerStage3(db *gorm.DB) error {
	return finalizeOwnerAndCreatorNotNull(db)
}

// MigrateOwnerStage4 drops legacy user_id/device_id columns. Called in T10.
func MigrateOwnerStage4(db *gorm.DB) error {
	return dropLegacyOwnerColumns(db)
}

// Stage 1: add new nullable columns (with defaults).
func addOwnerAndCreatorColumns(db *gorm.DB) error {
	if err := db.Exec(`
		ALTER TABLE sessions
		ADD COLUMN IF NOT EXISTS owner_kind VARCHAR(32),
		ADD COLUMN IF NOT EXISTS owner_ref_id VARCHAR(255)
	`).Error; err != nil {
		return err
	}
	if err := db.Exec(`
		ALTER TABLE messages
		ADD COLUMN IF NOT EXISTS creator_kind VARCHAR(32),
		ADD COLUMN IF NOT EXISTS creator_ref_id VARCHAR(255)
	`).Error; err != nil {
		return err
	}
	return nil
}

// Stage 2: backfill existing data (only when user_id column exists).
func backfillOwnerAndCreator(db *gorm.DB) error {
	// Check whether the user_id column exists (new databases may not have it).
	var hasUserID bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = 'user_id'
		)
	`).Scan(&hasUserID).Error; err != nil {
		return err
	}

	if hasUserID {
		// Backfill sessions: owner_ref_id IS NULL means not yet backfilled
		// (owner_kind may already have a default set by AutoMigrate).
		// Use batched updates to avoid long locks and WAL bloat on large tables.
		const batchSize = 1000
		var totalSessionsBackfilled int64
		var sessionBatchNum int
		for {
			sessionBatchNum++
			result := db.Exec(`
				UPDATE sessions
				SET owner_kind = 'user', owner_ref_id = user_id::text
				WHERE owner_ref_id IS NULL
				AND id IN (SELECT id FROM sessions WHERE owner_ref_id IS NULL LIMIT ?)
			`, batchSize)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				break
			}
			totalSessionsBackfilled += result.RowsAffected
			logger.Info(context.Background(), "backfill sessions batch completed",
				zap.Int("batch_number", sessionBatchNum),
				zap.Int64("rows_affected", result.RowsAffected),
				zap.Int64("total_backfilled", totalSessionsBackfilled),
			)
			// Sleep between batches to reduce lock contention and allow other operations
			// to proceed. Duration is a conservative default; see migrate.go for the
			// configurability TODO.
			time.Sleep(100 * time.Millisecond)
		}
		if totalSessionsBackfilled > 0 {
			logger.Info(context.Background(), "sessions backfill completed",
				zap.Int64("total_rows_backfilled", totalSessionsBackfilled),
				zap.Int("total_batches", sessionBatchNum-1),
			)
		}

		// Backfill messages: creator_ref_id IS NULL means not yet backfilled.
		// Use batched updates to avoid long locks and WAL bloat on large tables.
		var totalMessagesBackfilled int64
		var messageBatchNum int
		for {
			messageBatchNum++
			result := db.Exec(`
				UPDATE messages
				SET creator_kind = 'user', creator_ref_id = s.owner_ref_id
				FROM sessions s
				WHERE messages.session_id = s.id AND messages.creator_ref_id IS NULL
				AND messages.id IN (
					SELECT m.id FROM messages m
					JOIN sessions s2 ON m.session_id = s2.id
					WHERE m.creator_ref_id IS NULL
					LIMIT ?
				)
			`, batchSize)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				break
			}
			totalMessagesBackfilled += result.RowsAffected
			logger.Info(context.Background(), "backfill messages batch completed",
				zap.Int("batch_number", messageBatchNum),
				zap.Int64("rows_affected", result.RowsAffected),
				zap.Int64("total_backfilled", totalMessagesBackfilled),
			)
			// Sleep between batches to reduce lock contention and allow other operations
			// to proceed. Duration is a conservative default; see migrate.go for the
			// configurability TODO.
			time.Sleep(100 * time.Millisecond)
		}
		if totalMessagesBackfilled > 0 {
			logger.Info(context.Background(), "messages backfill completed",
				zap.Int64("total_rows_backfilled", totalMessagesBackfilled),
				zap.Int("total_batches", messageBatchNum-1),
			)
		}
	}
	return nil
}

// Stage 3: set NOT NULL constraints using PostgreSQL's NOT VALID constraint pattern.
// This approach minimizes lock contention on large tables by:
//  1. Adding CHECK constraints with NOT VALID (doesn't scan existing rows)
//  2. Validating constraints separately (can run concurrently with other operations)
//  3. Converting to NOT NULL after validation (fast, as PostgreSQL trusts the validated constraint)
func finalizeOwnerAndCreatorNotNull(db *gorm.DB) error {
	ctx := context.Background()

	// Step 1: Add CHECK constraints with NOT VALID for sessions table
	// NOT VALID means PostgreSQL won't scan existing rows to verify the constraint
	logger.Info(ctx, "owner.migration.stage3.step1_add_not_valid_constraints")

	if err := db.Exec(`
		ALTER TABLE sessions
		ADD CONSTRAINT chk_sessions_owner_kind_not_null
		CHECK (owner_kind IS NOT NULL) NOT VALID
	`).Error; err != nil {
		// Check if constraint already exists (idempotency)
		if !isConstraintExistsError(err) {
			return fmt.Errorf("add sessions owner_kind NOT VALID constraint: %w", err)
		}
		logger.Info(ctx, "owner.migration.stage3.constraint_already_exists", zap.String("table", "sessions"), zap.String("constraint", "chk_sessions_owner_kind_not_null"))
	}

	if err := db.Exec(`
		ALTER TABLE sessions
		ADD CONSTRAINT chk_sessions_owner_ref_id_not_null
		CHECK (owner_ref_id IS NOT NULL) NOT VALID
	`).Error; err != nil {
		if !isConstraintExistsError(err) {
			return fmt.Errorf("add sessions owner_ref_id NOT VALID constraint: %w", err)
		}
		logger.Info(ctx, "owner.migration.stage3.constraint_already_exists", zap.String("table", "sessions"), zap.String("constraint", "chk_sessions_owner_ref_id_not_null"))
	}

	// Step 2: Add CHECK constraints with NOT VALID for messages table
	if err := db.Exec(`
		ALTER TABLE messages
		ADD CONSTRAINT chk_messages_creator_kind_not_null
		CHECK (creator_kind IS NOT NULL) NOT VALID
	`).Error; err != nil {
		if !isConstraintExistsError(err) {
			return fmt.Errorf("add messages creator_kind NOT VALID constraint: %w", err)
		}
		logger.Info(ctx, "owner.migration.stage3.constraint_already_exists", zap.String("table", "messages"), zap.String("constraint", "chk_messages_creator_kind_not_null"))
	}

	if err := db.Exec(`
		ALTER TABLE messages
		ADD CONSTRAINT chk_messages_creator_ref_id_not_null
		CHECK (creator_ref_id IS NOT NULL) NOT VALID
	`).Error; err != nil {
		if !isConstraintExistsError(err) {
			return fmt.Errorf("add messages creator_ref_id NOT VALID constraint: %w", err)
		}
		logger.Info(ctx, "owner.migration.stage3.constraint_already_exists", zap.String("table", "messages"), zap.String("constraint", "chk_messages_creator_ref_id_not_null"))
	}

	// Step 3: Validate constraints (can run concurrently with other operations)
	// VALIDATE CONSTRAINT acquires a SHARE UPDATE EXCLUSIVE lock, which doesn't block reads/writes
	logger.Info(ctx, "owner.migration.stage3.step2_validate_constraints")

	if err := db.Exec(`VALIDATE CONSTRAINT chk_sessions_owner_kind_not_null ON sessions`).Error; err != nil {
		return fmt.Errorf("validate sessions owner_kind constraint: %w", err)
	}

	if err := db.Exec(`VALIDATE CONSTRAINT chk_sessions_owner_ref_id_not_null ON sessions`).Error; err != nil {
		return fmt.Errorf("validate sessions owner_ref_id constraint: %w", err)
	}

	if err := db.Exec(`VALIDATE CONSTRAINT chk_messages_creator_kind_not_null ON messages`).Error; err != nil {
		return fmt.Errorf("validate messages creator_kind constraint: %w", err)
	}

	if err := db.Exec(`VALIDATE CONSTRAINT chk_messages_creator_ref_id_not_null ON messages`).Error; err != nil {
		return fmt.Errorf("validate messages creator_ref_id constraint: %w", err)
	}

	// Step 4: Convert CHECK constraints to NOT NULL columns
	// After validation, PostgreSQL knows all rows satisfy the constraint, so this is fast
	// and only requires a brief ACCESS EXCLUSIVE lock
	logger.Info(ctx, "owner.migration.stage3.step3_convert_to_not_null")

	if err := db.Exec(`
		ALTER TABLE sessions
		ALTER COLUMN owner_kind SET NOT NULL,
		ALTER COLUMN owner_ref_id SET NOT NULL
	`).Error; err != nil {
		return fmt.Errorf("set sessions columns NOT NULL: %w", err)
	}

	if err := db.Exec(`
		ALTER TABLE messages
		ALTER COLUMN creator_kind SET NOT NULL,
		ALTER COLUMN creator_ref_id SET NOT NULL
	`).Error; err != nil {
		return fmt.Errorf("set messages columns NOT NULL: %w", err)
	}

	// Step 5: Drop CHECK constraints (no longer needed after converting to NOT NULL)
	logger.Info(ctx, "owner.migration.stage3.step4_drop_check_constraints")

	if err := db.Exec(`
		ALTER TABLE sessions
		DROP CONSTRAINT IF EXISTS chk_sessions_owner_kind_not_null,
		DROP CONSTRAINT IF EXISTS chk_sessions_owner_ref_id_not_null
	`).Error; err != nil {
		return fmt.Errorf("drop sessions check constraints: %w", err)
	}

	if err := db.Exec(`
		ALTER TABLE messages
		DROP CONSTRAINT IF EXISTS chk_messages_creator_kind_not_null,
		DROP CONSTRAINT IF EXISTS chk_messages_creator_ref_id_not_null
	`).Error; err != nil {
		return fmt.Errorf("drop messages check constraints: %w", err)
	}

	logger.Info(ctx, "owner.migration.stage3.completed")
	return nil
}

// isConstraintExistsError checks if the error is due to a constraint already existing.
// PostgreSQL error code 42710 is "duplicate_object" (constraint already exists).
func isConstraintExistsError(err error) bool {
	if err == nil {
		return false
	}
	// Check for PostgreSQL error code 42710 (duplicate_object)
	return err.Error() != "" && (containsString(err.Error(), "42710") || containsString(err.Error(), "already exists"))
}

// containsString checks if s contains substr (simple helper to avoid importing strings package).
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsSubstring(s, substr)))
}

// containsSubstring is a simple substring search.
func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Stage 4: drop legacy columns (only user_id; device_id is kept for protocol compatibility).
func dropLegacyOwnerColumns(db *gorm.DB) error {
	if err := db.Exec(`
		ALTER TABLE sessions DROP COLUMN IF EXISTS user_id;
	`).Error; err != nil {
		return err
	}
	return nil
}
