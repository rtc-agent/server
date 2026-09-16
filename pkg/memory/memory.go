// Package memory 定义统一 Memory 模型的领域层。
//
// Memory 是 OKF (Open Knowledge Format) 兼容的知识单元，通过 Scope 字段区分作用域：
// session（会话级）、user（用户级）、global（全局级）。
// 本包包含领域模型、验证逻辑、Repository 接口定义和格式化器，
// 不依赖具体存储实现，可由 agent、API、CLI 等多方消费。
package memory

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ScopeType 定义 Memory 的作用域
type ScopeType string

const (
	ScopeSession ScopeType = "session" // 会话级，随 session 结束归档
	ScopeUser    ScopeType = "user"    // 用户级，跨 session 持久化
	ScopeGlobal  ScopeType = "global"  // 全局级，所有用户共享
)

// ValidScopeTypes 所有有效的作用域
var ValidScopeTypes = []ScopeType{ScopeSession, ScopeUser, ScopeGlobal}

// IsValidScopeType 检查作用域是否有效
func IsValidScopeType(scope ScopeType) bool {
	for _, s := range ValidScopeTypes {
		if s == scope {
			return true
		}
	}
	return false
}

// ValidMemoryTypes 所有有效的 Memory 类型
var ValidMemoryTypes = []string{
	"decision", "context", "progress", "issue", "learnings",
	"user", "feedback", "project", "reference",
}

// IsValidMemoryType 检查类型是否有效
func IsValidMemoryType(t string) bool {
	for _, v := range ValidMemoryTypes {
		if v == t {
			return true
		}
	}
	return false
}

// Memory 是 OKF 兼容的知识单元
type Memory struct {
	ID uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`

	// ─── 作用域 ───
	Scope   ScopeType `json:"scope" gorm:"type:varchar(20);not null;index"`
	ScopeID uuid.UUID `json:"scopeId" gorm:"type:uuid;index"` // session_id / user_id / zero for global

	// ─── OKF 标准字段 ───
	Type        string `json:"type" gorm:"type:varchar(50);not null;index"` // decision | context | progress | issue | learnings | ...
	Title       string `json:"title" gorm:"type:varchar(200);not null"`     // 5-10 词摘要
	Description string `json:"description" gorm:"type:varchar(500)"`        // 一句话描述
	Content     string `json:"content" gorm:"type:text;not null"`           // markdown body
	// Tags 使用 JSONB 数组而非 PostgreSQL text[]，因为：
	// 1. JSONB 更灵活，支持嵌套结构（未来扩展）
	// 2. 与 Metadata 字段保持一致的存储策略
	// 3. 查询性能差异在标签数量少的场景下可忽略
	// 注意：与 UserMemory.Tags (text[]) 不一致，迁移时需要转换。
	Tags      StringArray `json:"tags" gorm:"type:jsonb;default:'[]'"` // 标签数组
	Resource  string      `json:"resource" gorm:"type:varchar(500)"`   // 外部链接
	Timestamp time.Time   `json:"timestamp" gorm:"not null"`           // 知识时间戳

	// ─── 扩展 ───
	Metadata   JSONBString `json:"metadata" gorm:"type:jsonb;default:'{}'"` // OKF Provenance/Trust/Lifecycle
	TokenCount int         `json:"tokenCount" gorm:"default:0"`             // 预估 token

	// ─── 审计 ───
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// DeletedAt 使用 gorm.DeletedAt 而非 *time.Time，因为：
	// 1. GORM 自动处理软删除过滤，减少手动 WHERE 条件
	// 2. 与 GORM 生态更一致，便于使用 Unscoped() 等 API
	// 注意：与现有 SessionMemory/UserMemory 的 *time.Time 不一致，
	// 迁移时需要统一或编写适配层。
	DeletedAt gorm.DeletedAt `json:"deletedAt" gorm:"index"`
}

// TableName 指定表名
func (Memory) TableName() string {
	return "memories"
}

// BeforeCreate 自动生成 UUID v7
func (m *Memory) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// Validate 验证模型字段
func (m *Memory) Validate() error {
	if !IsValidScopeType(m.Scope) {
		return fmt.Errorf("%w: %q", ErrInvalidScope, m.Scope)
	}
	if !IsValidMemoryType(m.Type) {
		return fmt.Errorf("%w: %q", ErrInvalidType, m.Type)
	}
	if m.Title == "" {
		return fmt.Errorf("title: %w", ErrRequiredField)
	}
	if m.Content == "" {
		return fmt.Errorf("content: %w", ErrRequiredField)
	}
	if m.Timestamp.IsZero() {
		return fmt.Errorf("timestamp: %w", ErrRequiredField)
	}
	// ScopeID 要求：session 和 user scope 必须有 ScopeID
	if (m.Scope == ScopeSession || m.Scope == ScopeUser) && m.ScopeID == uuid.Nil {
		return fmt.Errorf("scopeId is required for %s scope", m.Scope)
	}
	return nil
}

// ─── MemoryLink ───

// MemoryLink 表示概念间的语义关系
type MemoryLink struct {
	ID       uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	FromID   uuid.UUID `json:"fromId" gorm:"type:uuid;not null;index"`
	ToID     uuid.UUID `json:"toId" gorm:"type:uuid;not null;index"`
	Relation string    `json:"relation" gorm:"type:varchar(50);not null"` // related | depends_on | supersedes | derives_from

	CreatedAt time.Time `json:"createdAt"`
}

// TableName 指定表名
func (MemoryLink) TableName() string {
	return "memory_links"
}

// BeforeCreate 自动生成 UUID v7
func (l *MemoryLink) BeforeCreate(tx *gorm.DB) error {
	if l.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		l.ID = id
	}
	return nil
}

// ValidRelations 所有有效的关系类型
var ValidRelations = []string{"related", "depends_on", "supersedes", "derives_from"}

// IsValidRelation 检查关系类型是否有效
func IsValidRelation(relation string) bool {
	for _, r := range ValidRelations {
		if r == relation {
			return true
		}
	}
	return false
}

// Validate 验证 link 字段
func (l *MemoryLink) Validate() error {
	if l.FromID == uuid.Nil {
		return fmt.Errorf("fromId is required")
	}
	if l.ToID == uuid.Nil {
		return fmt.Errorf("toId is required")
	}
	if l.FromID == l.ToID {
		return fmt.Errorf("fromId and toId must be different")
	}
	if !IsValidRelation(l.Relation) {
		return fmt.Errorf("invalid relation: %q", l.Relation)
	}
	return nil
}
