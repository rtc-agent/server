package model

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserMemory 用户级记忆模型
// 用于跨会话保持用户偏好、项目信息、技术栈等长期知识。
type UserMemory struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`

	// 分类（对齐 Claude Code）: user/feedback/project/reference
	Category   string `gorm:"size:50;not null;index" json:"category"`
	Importance string `gorm:"size:20;not null;default:'medium';index" json:"importance"` // low/medium/high/critical

	// 内容
	Title       string  `gorm:"size:200;not null" json:"title"`
	Content     string  `gorm:"type:text;not null" json:"content"`
	Description *string `gorm:"size:500" json:"description,omitempty"` // 一行描述（用于检索）

	// 标签和向量
	Tags      StringArray  `gorm:"type:text[]" json:"tags,omitempty"` // 标签（用于关键词匹配）
	Embedding VectorArray  `gorm:"type:vector(1536)" json:"-"`        // Embedding 向量（不序列化到 JSON）

	// 元数据
	Metadata        JSONB[any]  `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	SourceSessionID *uuid.UUID  `gorm:"type:uuid" json:"source_session_id,omitempty"`

	// 时间戳
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `gorm:"index" json:"-"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`

	// 检索统计
	AccessCount int `gorm:"default:0" json:"access_count"`
}

func (m *UserMemory) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m.ID = id
	}
	return nil
}

// User Memory 分类常量（对齐 Claude Code）
const (
	UserMemoryCategoryUser      = "user"
	UserMemoryCategoryFeedback  = "feedback"
	UserMemoryCategoryProject   = "project"
	UserMemoryCategoryReference = "reference"
)

// ValidUserMemoryCategories 所有有效的分类
var ValidUserMemoryCategories = []string{
	UserMemoryCategoryUser,
	UserMemoryCategoryFeedback,
	UserMemoryCategoryProject,
	UserMemoryCategoryReference,
}

// IsValidUserMemoryCategory 检查分类是否有效
func IsValidUserMemoryCategory(category string) bool {
	for _, c := range ValidUserMemoryCategories {
		if c == category {
			return true
		}
	}
	return false
}

// Importance 级别常量
const (
	ImportanceLow      = "low"
	ImportanceMedium   = "medium"
	ImportanceHigh     = "high"
	ImportanceCritical = "critical"
)

// ValidImportances 所有有效的重要性级别
var ValidImportances = []string{
	ImportanceLow,
	ImportanceMedium,
	ImportanceHigh,
	ImportanceCritical,
}

// IsValidImportance 检查重要性级别是否有效
func IsValidImportance(importance string) bool {
	for _, i := range ValidImportances {
		if i == importance {
			return true
		}
	}
	return false
}

// ImportanceWeight 返回重要性级别的权重（用于检索排序）
func ImportanceWeight(importance string) float64 {
	switch importance {
	case ImportanceCritical:
		return 1.5
	case ImportanceHigh:
		return 1.3
	case ImportanceMedium:
		return 1.0
	case ImportanceLow:
		return 0.7
	default:
		return 1.0
	}
}

// VectorArray 是 []float32 的自定义类型，用于 GORM 的 vector 类型（pgvector）
type VectorArray []float32

// Value 实现 driver.Valuer，将 VectorArray 转换为 pgvector 格式字符串
// 例如: [1.0, 2.0, 3.0] -> "[1.0,2.0,3.0]"
func (v VectorArray) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	if len(v) == 0 {
		return "[]", nil
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%g", f)
	}
	sb.WriteByte(']')
	return sb.String(), nil
}

// Scan 实现 sql.Scanner，从 pgvector 格式字符串解析为 VectorArray
// 输入格式: "[1.0,2.0,3.0]" 或 "[1,2,3]"
func (v *VectorArray) Scan(src any) error {
	if src == nil {
		*v = nil
		return nil
	}
	var s string
	switch val := src.(type) {
	case string:
		s = val
	case []byte:
		s = string(val)
	default:
		return fmt.Errorf("VectorArray.Scan: unsupported type %T", src)
	}

	// 去除方括号
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return fmt.Errorf("VectorArray.Scan: invalid format: %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		*v = VectorArray{}
		return nil
	}

	parts := strings.Split(inner, ",")
	result := make(VectorArray, len(parts))
	for i, p := range parts {
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%g", &f); err != nil {
			return fmt.Errorf("VectorArray.Scan: parse element %d: %w", i, err)
		}
		result[i] = float32(f)
	}
	*v = result
	return nil
}
