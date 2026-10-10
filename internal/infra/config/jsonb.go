package config

import (
	"encoding/json"

	"gorm.io/datatypes"
)

// UnmarshalJSONB safely unmarshals datatypes.JSON to a Go value.
// This is a common implementation used across the codebase to avoid duplication.
//
// If the input is empty, it returns nil.
// If unmarshaling fails, it returns the raw string representation.
// Otherwise, it returns the unmarshaled Go value (any type).
func UnmarshalJSONB(data datatypes.JSON) any {
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	return v
}
