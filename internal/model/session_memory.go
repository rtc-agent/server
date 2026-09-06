package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SessionMemory 会话记忆模型
// 用于在会话过程中持续提取关键信息，为自动压缩提供摘要内容。
type SessionMemory struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	SessionID uuid.UUID `gorm:"type:uuid;not null;index" json:"session_id"`

	// 分类: decision/context/progress/issue/learnings
	Category string `gorm:"size:50;not null;index" json:"category"`

	// 内容
	Title   string `gorm:"size:200;not null" json:"title"`
	Content string `gorm:"type:text;not null" json:"content"`

	// 元数据（JSON 格式，存储相关文件、代码片段等）
	Metadata JSONB[any] `gorm:"type:jsonb;default:'{}'" json:"metadata"`

	// 预估 token 数（用于限制总大小）
	TokenCount *int `json:"token_count,omitempty"`

	// 时间戳
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `gorm:"index" json:"-"`
}

func (m *SessionMemory) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// Session Memory 分类常量
const (
	SessionMemoryCategoryDecision  = "decision"
	SessionMemoryCategoryContext   = "context"
	SessionMemoryCategoryProgress  = "progress"
	SessionMemoryCategoryIssue     = "issue"
	SessionMemoryCategoryLearnings = "learnings"
)

// ValidCategories 所有有效的分类
var ValidSessionMemoryCategories = []string{
	SessionMemoryCategoryDecision,
	SessionMemoryCategoryContext,
	SessionMemoryCategoryProgress,
	SessionMemoryCategoryIssue,
	SessionMemoryCategoryLearnings,
}

// IsValidCategory 检查分类是否有效
func IsValidCategory(category string) bool {
	for _, c := range ValidSessionMemoryCategories {
		if c == category {
			return true
		}
	}
	return false
}

// ToProtocolSessionMemory 将 model.SessionMemory 转换为 protocol.SessionMemory。
// 注意：当前 protocol 中没有 SessionMemory 的定义，这里预留用于未来扩展。
// 如果需要暴露给客户端，需要在 protocol 中添加相应的类型定义。
