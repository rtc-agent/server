package model

import (
	"fmt"

	"gorm.io/gorm"
)

// MigrateServerConfigTable migrates the server_configs table from composite primary key
// (key, user_id) to surrogate primary key (id). This migration is idempotent and safe
// to run multiple times.
//
// Migration steps:
//  1. Add id column (UUID) if not exists
//  2. Backfill existing rows with UUIDs using gen_random_uuid()
//  3. Drop the old composite primary key constraint (dynamically queried)
//  4. Set id as NOT NULL and add primary key constraint
//  5. Make user_id column nullable
//
// The entire migration runs in a transaction for atomicity.
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

	// Run migration in a transaction
	return db.Transaction(func(tx *gorm.DB) error {
		// Step 1: Add id column if not exists
		if !hasIDColumn {
			if err := tx.Exec(`
				ALTER TABLE server_configs
				ADD COLUMN IF NOT EXISTS id UUID
			`).Error; err != nil {
				return fmt.Errorf("add id column: %w", err)
			}
		}

		// Step 2: Backfill existing rows with UUIDs
		if err := tx.Exec(`
			UPDATE server_configs
			SET id = gen_random_uuid()
			WHERE id IS NULL
		`).Error; err != nil {
			return fmt.Errorf("backfill id column: %w", err)
		}

		// Step 3: Drop the old composite primary key constraint (if exists)
		// Query the constraint name dynamically
		var constraintName string
		if err := tx.Raw(`
			SELECT tc.constraint_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON tc.constraint_name = kcu.constraint_name
			WHERE tc.table_name = 'server_configs'
			  AND tc.constraint_type = 'PRIMARY KEY'
		`).Scan(&constraintName).Error; err != nil {
			return fmt.Errorf("query primary key constraint: %w", err)
		}

		if constraintName != "" {
			// Drop the constraint using dynamic SQL
			if err := tx.Exec(fmt.Sprintf(
				`ALTER TABLE server_configs DROP CONSTRAINT IF EXISTS %s`,
				constraintName,
			)).Error; err != nil {
				return fmt.Errorf("drop primary key constraint %s: %w", constraintName, err)
			}
		}

		// Step 4: Set id as NOT NULL and add primary key
		if err := tx.Exec(`
			ALTER TABLE server_configs
			ALTER COLUMN id SET NOT NULL,
			ADD PRIMARY KEY (id)
		`).Error; err != nil {
			return fmt.Errorf("set id as primary key: %w", err)
		}

		// Step 5: Make user_id nullable
		if err := tx.Exec(`
			ALTER TABLE server_configs
			ALTER COLUMN user_id DROP NOT NULL
		`).Error; err != nil {
			return fmt.Errorf("make user_id nullable: %w", err)
		}

		return nil
	})
}
