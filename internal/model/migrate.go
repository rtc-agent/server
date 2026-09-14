package model

import (
	"gorm.io/gorm"

	"github.com/rtc-agent/server/pkg/memory"
)

// AutoMigrate 自动迁移所有模型
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
		// Phase 2: 统一 Memory 模型
		&memory.Memory{},
		&memory.MemoryLink{},
	); err != nil {
		return err
	}

	// 为 memory_links 添加复合索引，优化按 from_id + relation 查询
	db.Exec("CREATE INDEX IF NOT EXISTS idx_memory_links_from_relation ON memory_links(from_id, relation)")

	return nil
}
