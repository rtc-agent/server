package memory

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// StringArray is a JSONB-serializable []string type.
// Behavior matches model.StringArray but is defined independently in
// pkg/memory to avoid an architectural violation (pkg depending on internal).
type StringArray []string

// Value implements driver.Valuer, serializing the string slice to JSON.
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

// Scan implements sql.Scanner, deserializing JSON back into the string slice.
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

// JSONBString is a string type for optional JSONB columns.
// Empty string maps to SQL NULL (avoiding PostgreSQL rejecting "" as
// invalid JSONB); non-empty strings are written as-is (the caller must
// ensure valid JSON). On read, SQL NULL maps to empty string and non-NULL
// maps to the raw JSON text.
type JSONBString string

// Value implements driver.Valuer: empty string returns nil (SQL NULL),
// otherwise returns the raw string.
func (j JSONBString) Value() (driver.Value, error) {
	if j == "" {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner: SQL NULL maps to empty string, otherwise
// converts to string.
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
