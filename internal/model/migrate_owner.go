package model

import (
	"context"
	"fmt"
	"strings"
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
// NOTE: PostgreSQL UPDATE statements are atomic - each batch UPDATE either succeeds
// completely or fails completely. There is no risk of partial updates within a batch.
// If the migration crashes between batches, the next run will resume from where it
// left off (idempotent via WHERE owner_ref_id IS NULL condition).
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

	// Step 1: Add CHECK constraints with NOT VALID for sessions table.
	// NOT VALID means PostgreSQL won't scan existing rows to verify the constraint.
	logger.Info(ctx, "owner.migration.stage3.step1_add_not_valid_constraints")

	sessionsConstraints := make([]columnConstraint, 0, 2)
	sessionsConstraints = append(sessionsConstraints,
		columnConstraint{Table: "sessions", Constraint: "chk_sessions_owner_kind_not_null", Column: "owner_kind"},
		columnConstraint{Table: "sessions", Constraint: "chk_sessions_owner_ref_id_not_null", Column: "owner_ref_id"},
	)
	messagesConstraints := make([]columnConstraint, 0, 2)
	messagesConstraints = append(messagesConstraints,
		columnConstraint{Table: "messages", Constraint: "chk_messages_creator_kind_not_null", Column: "creator_kind"},
		columnConstraint{Table: "messages", Constraint: "chk_messages_creator_ref_id_not_null", Column: "creator_ref_id"},
	)
	allConstraints := make([]columnConstraint, 0, len(sessionsConstraints)+len(messagesConstraints))
	allConstraints = append(allConstraints, sessionsConstraints...)
	allConstraints = append(allConstraints, messagesConstraints...)

	// Step 1: Add CHECK constraints with NOT VALID (fast, no table scan).
	for _, c := range allConstraints {
		if err := addCheckConstraint(ctx, db, c); err != nil {
			return err
		}
	}

	// Step 2: Validate constraints (SHARE UPDATE EXCLUSIVE lock, does not block reads/writes).
	// Idempotent: skip if constraint is already validated.
	logger.Info(ctx, "owner.migration.stage3.step2_validate_constraints")
	for _, c := range allConstraints {
		if err := validateConstraintIfNeeded(ctx, db, c); err != nil {
			return err
		}
	}

	// Step 3: Convert CHECK constraints to NOT NULL columns.
	// After validation, PostgreSQL knows all rows satisfy the constraint, so this is fast
	// and only requires a brief ACCESS EXCLUSIVE lock.
	logger.Info(ctx, "owner.migration.stage3.step3_convert_to_not_null")
	if err := setColumnsNotNull(db, "sessions", []string{"owner_kind", "owner_ref_id"}); err != nil {
		return err
	}
	if err := setColumnsNotNull(db, "messages", []string{"creator_kind", "creator_ref_id"}); err != nil {
		return err
	}

	// Step 4: Drop CHECK constraints (no longer needed after converting to NOT NULL).
	logger.Info(ctx, "owner.migration.stage3.step4_drop_check_constraints")
	if err := dropCheckConstraints(db, "sessions", []string{"chk_sessions_owner_kind_not_null", "chk_sessions_owner_ref_id_not_null"}); err != nil {
		return err
	}
	if err := dropCheckConstraints(db, "messages", []string{"chk_messages_creator_kind_not_null", "chk_messages_creator_ref_id_not_null"}); err != nil {
		return err
	}

	logger.Info(ctx, "owner.migration.stage3.completed")
	return nil
}

// columnConstraint identifies a CHECK constraint by table/constraint/column names.
type columnConstraint struct {
	Table      string
	Constraint string
	Column     string
}

// addCheckConstraint adds a CHECK (column IS NOT NULL) NOT VALID constraint to the table.
// Idempotent: silently succeeds if the constraint already exists.
func addCheckConstraint(ctx context.Context, db *gorm.DB, c columnConstraint) error {
	err := db.Exec(fmt.Sprintf(`
		ALTER TABLE %s
		ADD CONSTRAINT %s
		CHECK (%s IS NOT NULL) NOT VALID
	`, c.Table, c.Constraint, c.Column)).Error
	if err == nil {
		return nil
	}
	if !isConstraintExistsError(err) {
		return fmt.Errorf("add %s %s NOT VALID constraint: %w", c.Table, c.Constraint, err)
	}
	// Constraint exists - check whether it has already been validated.
	var isValidated bool
	if queryErr := db.Raw(`
		SELECT convalidated
		FROM pg_constraint
		WHERE conname = ? AND conrelid = ?::regclass
	`, c.Constraint, c.Table).Scan(&isValidated).Error; queryErr == nil && isValidated {
		logger.Info(ctx, "owner.migration.stage3.constraint_already_validated",
			zap.String("table", c.Table),
			zap.String("constraint", c.Constraint))
		return nil
	}
	logger.Info(ctx, "owner.migration.stage3.constraint_already_exists",
		zap.String("table", c.Table),
		zap.String("constraint", c.Constraint))
	return nil
}

// validateConstraintIfNeeded runs VALIDATE CONSTRAINT only if it has not been validated yet.
func validateConstraintIfNeeded(ctx context.Context, db *gorm.DB, c columnConstraint) error {
	var needsValidation bool
	if err := db.Raw(`
		SELECT NOT convalidated
		FROM pg_constraint
		WHERE conname = ? AND conrelid = ?::regclass
	`, c.Constraint, c.Table).Scan(&needsValidation).Error; err != nil {
		// Constraint might not exist yet, which is fine - we'll create it in step 1.
		logger.Info(ctx, "owner.migration.stage3.constraint_check_skipped",
			zap.String("table", c.Table),
			zap.String("constraint", c.Constraint),
			zap.Error(err))
		return nil
	}
	if !needsValidation {
		logger.Info(ctx, "owner.migration.stage3.constraint_already_validated",
			zap.String("table", c.Table),
			zap.String("constraint", c.Constraint))
		return nil
	}
	if err := db.Exec(fmt.Sprintf(`ALTER TABLE %s VALIDATE CONSTRAINT %s`, c.Table, c.Constraint)).Error; err != nil {
		return fmt.Errorf("validate %s constraint on %s: %w", c.Constraint, c.Table, err)
	}
	return nil
}

// setColumnsNotNull issues ALTER COLUMN ... SET NOT NULL for each named column in a table.
func setColumnsNotNull(db *gorm.DB, table string, columns []string) error {
	setClauses := make([]string, 0, len(columns))
	for _, col := range columns {
		setClauses = append(setClauses, fmt.Sprintf("ALTER COLUMN %s SET NOT NULL", col))
	}
	query := fmt.Sprintf("ALTER TABLE %s %s", table, strings.Join(setClauses, ",\n"))
	if err := db.Exec(query).Error; err != nil {
		return fmt.Errorf("set %s columns NOT NULL: %w", table, err)
	}
	return nil
}

// dropCheckConstraints drops the named CHECK constraints from a table (IF EXISTS).
func dropCheckConstraints(db *gorm.DB, table string, constraints []string) error {
	if len(constraints) == 0 {
		return nil
	}
	clauses := make([]string, 0, len(constraints))
	for _, name := range constraints {
		clauses = append(clauses, fmt.Sprintf("DROP CONSTRAINT IF EXISTS %s", name))
	}
	query := fmt.Sprintf("ALTER TABLE %s %s", table, strings.Join(clauses, ",\n"))
	if err := db.Exec(query).Error; err != nil {
		return fmt.Errorf("drop %s check constraints: %w", table, err)
	}
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
