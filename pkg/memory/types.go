package memory

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// StringArray 是 []string 的 JSONB 序列化类型。
// 与 model.StringArray 行为一致，但在 pkg/memory 包内独立定义，
// 避免 pkg 层依赖 internal 层的架构违规。
type StringArray []string

func (s StringArray) Value() (driver.Value, error) {
	if s == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(s))
	if err != nil {
		return nil, fmt.Errorf("marshal StringArray: %w", err)
	}
	return string(b), nil
}

func (s *StringArray) Scan(src any) error {
	if src == nil {
		*s = StringArray{}
		return nil
	}
	var bytes []byte
	switch v := src.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	default:
		return fmt.Errorf("StringArray.Scan: unsupported type %T", src)
	}
	var arr []string
	if err := json.Unmarshal(bytes, &arr); err != nil {
		return fmt.Errorf("unmarshal StringArray: %w", err)
	}
	*s = arr
	return nil
}

// JSONBString 是可选 JSONB 列的字符串类型。
// 空字符串 -> SQL NULL（避免 PostgreSQL 拒绝 "" 作为无效 JSONB），
// 非空字符串 -> 原样写入（调用方须保证是合法 JSON）。
// 读取时 SQL NULL -> 空字符串，非 NULL -> 原始 JSON 文本。
type JSONBString string

// Value 实现 driver.Valuer：空字符串返回 nil（SQL NULL），否则返回原字符串。
func (j JSONBString) Value() (driver.Value, error) {
	if j == "" {
		return nil, nil
	}
	return string(j), nil
}

// Scan 实现 sql.Scanner：SQL NULL -> 空字符串，否则转为 string。
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
