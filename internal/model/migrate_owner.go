package model

import "gorm.io/gorm"

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
		if err := db.Exec(`
			UPDATE sessions
			SET owner_kind = 'user', owner_ref_id = user_id::text
			WHERE owner_ref_id IS NULL
		`).Error; err != nil {
			return err
		}
		// Backfill messages: creator_ref_id IS NULL means not yet backfilled.
		if err := db.Exec(`
			UPDATE messages
			SET creator_kind = 'user', creator_ref_id = s.owner_ref_id
			FROM sessions s
			WHERE messages.session_id = s.id AND messages.creator_ref_id IS NULL
		`).Error; err != nil {
			return err
		}
	}
	return nil
}

// Stage 3: set NOT NULL constraints.
func finalizeOwnerAndCreatorNotNull(db *gorm.DB) error {
	if err := db.Exec(`
		ALTER TABLE sessions
		ALTER COLUMN owner_kind SET NOT NULL,
		ALTER COLUMN owner_ref_id SET NOT NULL
	`).Error; err != nil {
		return err
	}
	if err := db.Exec(`
		ALTER TABLE messages
		ALTER COLUMN creator_kind SET NOT NULL,
		ALTER COLUMN creator_ref_id SET NOT NULL
	`).Error; err != nil {
		return err
	}
	return nil
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
