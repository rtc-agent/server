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
		// Use ctid-based batched updates to avoid index scans and reduce WAL bloat on large tables.
		const batchSize = 1000
		var totalSessionsBackfilled int64
		var sessionBatchNum int
		for {
			sessionBatchNum++
			result := db.Exec(`
				UPDATE sessions
				SET owner_kind = 'user', owner_ref_id = user_id::text
				WHERE owner_ref_id IS NULL
				AND ctid IN (SELECT ctid FROM sessions WHERE owner_ref_id IS NULL LIMIT ?)
			`, batchSize)
			if result.Error != nil {
				return fmt.Errorf("backfill sessions batch %d: %w", sessionBatchNum, result.Error)
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
		// Use ctid-based batched updates with simplified logic to reduce overhead.
		var totalMessagesBackfilled int64
		var messageBatchNum int
		for {
			messageBatchNum++
			result := db.Exec(`
				UPDATE messages
				SET creator_kind = 'user', creator_ref_id = s.owner_ref_id
				FROM sessions s
				WHERE messages.session_id = s.id AND messages.creator_ref_id IS NULL
				AND messages.ctid IN (
					SELECT m.ctid FROM messages m
					WHERE m.creator_ref_id IS NULL
					LIMIT ?
				)
			`, batchSize)
			if result.Error != nil {
				return fmt.Errorf("backfill messages batch %d: %w", messageBatchNum, result.Error)
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

	// Helper function to add constraint with idempotency check
	addCheckConstraint := func(tableName, constraintName, columnName string) error {
		err := db.Exec(fmt.Sprintf(`
			ALTER TABLE %s
			ADD CONSTRAINT %s
			CHECK (%s IS NOT NULL) NOT VALID
		`, tableName, constraintName, columnName)).Error
		if err != nil {
			// Check if constraint already exists (idempotency)
			if isConstraintExistsError(err) {
				// Constraint exists - check if it's already validated
				var isValidated bool
				if queryErr := db.Raw(`
					SELECT convalidated
					FROM pg_constraint
					WHERE conname = ? AND conrelid = ?::regclass
				`, constraintName, tableName).Scan(&isValidated).Error; queryErr == nil {
					if isValidated {
						logger.Info(ctx, "owner.migration.stage3.constraint_already_validated",
							zap.String("table", tableName),
							zap.String("constraint", constraintName))
						return nil
					}
				}
				logger.Info(ctx, "owner.migration.stage3.constraint_already_exists",
					zap.String("table", tableName),
					zap.String("constraint", constraintName))
				return nil
			}
			return fmt.Errorf("add %s %s NOT VALID constraint: %w", tableName, constraintName, err)
		}
		return nil
	}

	if err := addCheckConstraint("sessions", "chk_sessions_owner_kind_not_null", "owner_kind"); err != nil {
		return err
	}
	if err := addCheckConstraint("sessions", "chk_sessions_owner_ref_id_not_null", "owner_ref_id"); err != nil {
		return err
	}

	// Step 2: Add CHECK constraints with NOT VALID for messages table
	if err := addCheckConstraint("messages", "chk_messages_creator_kind_not_null", "creator_kind"); err != nil {
		return err
	}
	if err := addCheckConstraint("messages", "chk_messages_creator_ref_id_not_null", "creator_ref_id"); err != nil {
		return err
	}

	// Step 3: Validate constraints (can run concurrently with other operations)
	// VALIDATE CONSTRAINT acquires a SHARE UPDATE EXCLUSIVE lock, which doesn't block reads/writes
	// Check constraint status first for idempotency - skip if already validated
	logger.Info(ctx, "owner.migration.stage3.step2_validate_constraints")

	// Helper function to validate constraint only if not already valid
	validateConstraintIfNeeded := func(tableName, constraintName string) error {
		var needsValidation bool
		if err := db.Raw(`
			SELECT NOT convalidated
			FROM pg_constraint
			WHERE conname = ? AND conrelid = ?::regclass
		`, constraintName, tableName).Scan(&needsValidation).Error; err != nil {
			// Constraint might not exist yet, which is fine - we'll create it in step 1
			logger.Info(ctx, "owner.migration.stage3.constraint_check_skipped",
				zap.String("table", tableName),
				zap.String("constraint", constraintName),
				zap.Error(err))
			return nil
		}

		if !needsValidation {
			// Constraint is already validated, skip
			logger.Info(ctx, "owner.migration.stage3.constraint_already_validated",
				zap.String("table", tableName),
				zap.String("constraint", constraintName))
			return nil
		}

		// Validate the constraint
		if err := db.Exec(fmt.Sprintf(`VALIDATE CONSTRAINT %s ON %s`, constraintName, tableName)).Error; err != nil {
			return fmt.Errorf("validate %s constraint on %s: %w", constraintName, tableName, err)
		}
		return nil
	}

	if err := validateConstraintIfNeeded("sessions", "chk_sessions_owner_kind_not_null"); err != nil {
		return err
	}
	if err := validateConstraintIfNeeded("sessions", "chk_sessions_owner_ref_id_not_null"); err != nil {
		return err
	}
	if err := validateConstraintIfNeeded("messages", "chk_messages_creator_kind_not_null"); err != nil {
		return err
	}
	if err := validateConstraintIfNeeded("messages", "chk_messages_creator_ref_id_not_null"); err != nil {
		return err
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
	return containsErrorCode(err.Error(), "42710") || containsErrorCode(err.Error(), "already exists")
}

// containsErrorCode checks if the error message contains the specified error code or keyword.
// This is a simple substring search optimized for PostgreSQL error messages.
func containsErrorCode(s, code string) bool {
	if len(s) < len(code) {
		return false
	}
	for i := 0; i <= len(s)-len(code); i++ {
		if s[i:i+len(code)] == code {
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
