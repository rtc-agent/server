package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"github.com/rtc-agent/server/pkg/protocol"

	"github.com/google/uuid"
)

// JSONB is a generic JSONB column type, supporting PostgreSQL JSONB serialization for any Go element type.
// The underlying type is []T; Value serializes it to a JSON array, Scan deserializes from JSON array back to []T.
//
// Replaces the previously duplicated Value/Scan implementations of StringArray / UUIDArray / UpdateItemArray.
//
// Usage:
//
//	type UserUpdate struct {
//	    Items UpdateItemArray `gorm:"type:jsonb"`
//	}
//	// UpdateItemArray = JSONB[protocol.UpdateItem]
type JSONB[T any] []T

// Value implements driver.Valuer, serializing the JSONB slice to a JSON string
// for storage in PostgreSQL JSONB columns.
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

// Scan implements sql.Scanner, deserializing a JSON string from PostgreSQL
// back into the JSONB slice.
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

// ========== Concrete type aliases ==========

// StringArray is a string slice (JSONB storage).
type StringArray = JSONB[string]

// UUIDArray is a UUID slice (JSONB storage).
type UUIDArray = JSONB[uuid.UUID]

// UpdateItemArray is a protocol.UpdateItem slice (JSONB storage), used for UserUpdate.Items.
type UpdateItemArray = JSONB[protocol.UpdateItem]

// JSONBString is a string type for optional JSONB columns.
// Empty string -> SQL NULL (avoids PostgreSQL rejecting "" as invalid JSONB).
// Non-empty string -> written as-is (caller must ensure valid JSON).
// On read: SQL NULL -> empty string, non-NULL -> raw JSON text.
type JSONBString string

// Value implements driver.Valuer: empty string returns nil (SQL NULL), otherwise returns the original string.
func (j JSONBString) Value() (driver.Value, error) {
	if j == "" {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner: SQL NULL -> empty string, otherwise convert to string.
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
