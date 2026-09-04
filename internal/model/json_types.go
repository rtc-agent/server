package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// JSONB 是泛型 JSONB 列类型，支持任意 Go 元素类型的 PostgreSQL JSONB 序列化。
// 底层类型为 []T，Value 将其序列化为 JSON 数组，Scan 从 JSON 数组反序列化回 []T。
//
// 替代此前 StringArray / UUIDArray / UpdateItemArray 各自重复的 Value/Scan 实现。
//
// 用法：
//
//	type UserUpdate struct {
//	    Items UpdateItemArray `gorm:"type:jsonb"`
//	}
//	// UpdateItemArray = JSONB[protocol.UpdateItem]
type JSONB[T any] []T

func (j JSONB[T]) Value() (driver.Value, error) {
	if j == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]T(j))
	if err != nil {
		return nil, fmt.Errorf("marshal JSONB: %w", err)
	}
	return string(b), nil
}

func (j *JSONB[T]) Scan(src any) error {
	if src == nil {
		*j = JSONB[T]{}
		return nil
	}
	var bytes []byte
	switch v := src.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	default:
		return fmt.Errorf("JSONB.Scan: unsupported type %T", src)
	}
	var arr []T
	if err := json.Unmarshal(bytes, &arr); err != nil {
		return fmt.Errorf("unmarshal JSONB: %w", err)
	}
	*j = arr
	return nil
}

// ========== 具体类型别名 ==========

// StringArray 字符串切片（JSONB 存储）
type StringArray = JSONB[string]

// UUIDArray UUID 切片（JSONB 存储）
type UUIDArray = JSONB[uuid.UUID]

// UpdateItemArray protocol.UpdateItem 切片（JSONB 存储），用于 UserUpdate.Items
type UpdateItemArray = JSONB[protocol.UpdateItem]

// JSONBString 是可选 JSONB 列的字符串类型。
// 空字符串 → SQL NULL（避免 PostgreSQL 拒绝 ” 作为无效 JSONB），
// 非空字符串 → 原样写入（调用方须保证是合法 JSON）。
// 读取时 SQL NULL → 空字符串，非 NULL → 原始 JSON 文本。
type JSONBString string

// Value 实现 driver.Valuer：空字符串返回 nil（SQL NULL），否则返回原字符串。
func (j JSONBString) Value() (driver.Value, error) {
	if j == "" {
		return nil, nil
	}
	return string(j), nil
}

// Scan 实现 sql.Scanner：SQL NULL → 空字符串，否则转为 string。
func (j *JSONBString) Scan(src any) error {
	if src == nil {
		*j = ""
		return nil
	}
	switch v := src.(type) {
	case string:
		*j = JSONBString(v)
	case []byte:
		*j = JSONBString(v)
	default:
		return fmt.Errorf("JSONBString.Scan: unsupported type %T", src)
	}
	return nil
}
