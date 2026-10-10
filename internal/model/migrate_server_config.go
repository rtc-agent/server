package model

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/logger"
)

// MigrateServerConfigTable migrates the server_configs table from composite primary key
// (key, user_id) to surrogate primary key (id). This migration is idempotent and safe
// to run multiple times.
//
// Migration steps:
//  1. Add id column (UUID) if not exists
//  2. Backfill existing rows with UUIDs using gen_random_uuid() in batches
//  3. Drop the old composite primary key constraint (dynamically queried)
//  4. Set id as NOT NULL and add primary key constraint
//  5. Make user_id column nullable
//
// IMPORTANT: Production Deployment Considerations
//
// To minimize lock contention and avoid blocking reads/writes for extended periods,
// this migration uses a phased approach:
//
//   - Phase 1 (fast, low impact): Add nullable id column
//   - Phase 2 (batched, low impact): Backfill UUIDs in batches of 1000 rows with 100ms delays
//   - Phase 3 (fast, requires brief lock): Drop old PK, set NOT NULL, add new PK
//
// Phase 3 still requires ACCESS EXCLUSIVE locks, but it executes quickly because the heavy
// work (backfill) was done incrementally. For production environments with very large tables:
//  1. Schedule during low-traffic maintenance windows
//  2. Monitor pg_stat_activity for lock contention
//  3. Ensure statement_timeout is disabled (see migrate command)
func MigrateServerConfigTable(db *gorm.DB) error {
	// Check if table exists
	var tableExists bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'server_configs'
		)
	`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("check table existence: %w", err)
	}

	if !tableExists {
		// Table doesn't exist yet, will be created by AutoMigrate
		return nil
	}

	// Check if id column already exists
	var hasIDColumn bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'server_configs' AND column_name = 'id'
		)
	`).Scan(&hasIDColumn).Error; err != nil {
		return fmt.Errorf("check id column: %w", err)
	}

	if hasIDColumn {
		// Check if id is already a primary key
		var isPK bool
		if err := db.Raw(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.table_constraints tc
				JOIN information_schema.key_column_usage kcu
				  ON tc.constraint_name = kcu.constraint_name
				WHERE tc.table_name = 'server_configs'
				  AND tc.constraint_type = 'PRIMARY KEY'
				  AND kcu.column_name = 'id'
			)
		`).Scan(&isPK).Error; err != nil {
			return fmt.Errorf("check id primary key: %w", err)
		}

		if isPK {
			// Migration already complete
			return nil
		}
	}

	// Phase 1: Add id column if not exists (fast, minimal lock)
	if !hasIDColumn {
		logger.Info(context.Background(), "migrate_server_config.phase1_add_column")
		if err := db.Exec(`
			ALTER TABLE server_configs
			ADD COLUMN IF NOT EXISTS id UUID
		`).Error; err != nil {
			return fmt.Errorf("add id column: %w", err)
		}
	}

	// Phase 2: Backfill existing rows with UUIDs in batches (slow but low impact)
	logger.Info(context.Background(), "migrate_server_config.phase2_backfill_start")
	const batchSize = 1000
	var totalBackfilled int64
	var batchNum int

	for {
		batchNum++
		result := db.Exec(`
			UPDATE server_configs
			SET id = gen_random_uuid()
			WHERE id IS NULL
			AND ctid IN (SELECT ctid FROM server_configs WHERE id IS NULL LIMIT ?)
		`, batchSize)

		if result.Error != nil {
			return fmt.Errorf("backfill id column batch %d: %w", batchNum, result.Error)
		}

		if result.RowsAffected == 0 {
			break
		}

		totalBackfilled += result.RowsAffected
		logger.Info(context.Background(), "migrate_server_config.phase2_backfill_batch",
			zap.Int("batch_number", batchNum),
			zap.Int64("rows_affected", result.RowsAffected),
			zap.Int64("total_backfilled", totalBackfilled))

		// Sleep between batches to reduce lock contention and allow other operations
		time.Sleep(100 * time.Millisecond)
	}

	if totalBackfilled > 0 {
		logger.Info(context.Background(), "migrate_server_config.phase2_backfill_completed",
			zap.Int64("total_rows_backfilled", totalBackfilled),
			zap.Int("total_batches", batchNum-1))
	}

	// Phase 3: DDL operations (requires ACCESS EXCLUSIVE lock but executes quickly)
	logger.Info(context.Background(), "migrate_server_config.phase3_ddl_start")
	return db.Transaction(func(tx *gorm.DB) error {
		// Step 3a: Drop the old primary key constraint (if exists)
		// Query the constraint name dynamically. We already verified outside the transaction
		// that 'id' is not a PK, so any existing PK here is the old composite key.
		var pkConstraintName string
		if err := tx.Raw(`
			SELECT tc.constraint_name
			FROM information_schema.table_constraints tc
			WHERE tc.table_name = 'server_configs'
			  AND tc.constraint_type = 'PRIMARY KEY'
		`).Scan(&pkConstraintName).Error; err != nil {
			return fmt.Errorf("query primary key constraint: %w", err)
		}

		if pkConstraintName != "" {
			// Drop the existing primary key constraint.
			// We know it's not on 'id' because we checked earlier (lines 82-102).
			if err := tx.Exec(fmt.Sprintf(
				`ALTER TABLE server_configs DROP CONSTRAINT IF EXISTS %s`,
				pkConstraintName,
			)).Error; err != nil {
				return fmt.Errorf("drop primary key constraint %s: %w", pkConstraintName, err)
			}
		}

		// Step 3b: Set id as NOT NULL and add primary key
		if err := tx.Exec(`
			ALTER TABLE server_configs
			ALTER COLUMN id SET NOT NULL,
			ADD PRIMARY KEY (id)
		`).Error; err != nil {
			return fmt.Errorf("set id as primary key: %w", err)
		}

		// Step 3c: Make user_id nullable
		if err := tx.Exec(`
			ALTER TABLE server_configs
			ALTER COLUMN user_id DROP NOT NULL
		`).Error; err != nil {
			return fmt.Errorf("make user_id nullable: %w", err)
		}

		logger.Info(context.Background(), "migrate_server_config.phase3_ddl_completed")
		return nil
	})
}
