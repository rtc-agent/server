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
	); err != nil {
		return err
	}

	// Add composite index on memory_links to optimise queries by from_id + relation.
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_memory_links_from_relation ON memory_links(from_id, relation)").Error; err != nil {
		return fmt.Errorf("create index idx_memory_links_from_relation: %w", err)
	}

	return nil
}
