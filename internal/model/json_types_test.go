package model

import (
	"database/sql/driver"
	"testing"
)

func TestJSONBString_Value(t *testing.T) {
	tests := []struct {
		name     string
		input    JSONBString
		expected driver.Value
	}{
		{
			name:     "empty string returns nil (SQL NULL)",
			input:    "",
			expected: nil,
		},
		{
			name:     "valid JSON returns string",
			input:    `{"key":"value"}`,
			expected: `{"key":"value"}`,
		},
		{
			name:     "JSON array returns string",
			input:    `[1,2,3]`,
			expected: `[1,2,3]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.input.Value()
			if err != nil {
				t.Errorf("Value() unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("Value() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestJSONBString_Scan(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected JSONBString
		wantErr  bool
	}{
		{
			name:     "nil scans to empty string",
			input:    nil,
			expected: "",
		},
		{
			name:     "string scans correctly",
			input:    `{"key":"value"}`,
			expected: `{"key":"value"}`,
		},
		{
			name:     "byte slice scans correctly",
			input:    []byte(`[1,2,3]`),
			expected: `[1,2,3]`,
		},
		{
			name:    "unsupported type returns error",
			input:   123,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result JSONBString
			err := result.Scan(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("Scan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("Scan() = %v, want %v", result, tt.expected)
			}
		})
	}
}
