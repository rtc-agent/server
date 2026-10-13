package usecase

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/rtc-agent/server/internal/infra/config"
)

// This file contains tests for unexported functions in server_config.go.
// These tests are in the same package (usecase) to access unexported functions.

func TestUnmarshalJSONB(t *testing.T) {
	t.Run("returns nil for empty JSON", func(t *testing.T) {
		result := config.UnmarshalJSONB(nil)
		assert.Nil(t, result)

		result = config.UnmarshalJSONB([]byte{})
		assert.Nil(t, result)
	})

	t.Run("unmarshals string", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`"hello"`)))
		assert.Equal(t, "hello", result)
	})

	t.Run("unmarshals number", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`42`)))
		assert.Equal(t, float64(42), result)
	})

	t.Run("unmarshals object", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`{"key":"value"}`)))
		m, ok := result.(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "value", m["key"])
	})

	t.Run("unmarshals array", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`[1,2,3]`)))
		arr, ok := result.([]interface{})
		require.True(t, ok)
		assert.Len(t, arr, 3)
	})

	t.Run("returns string for invalid JSON", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`invalid`)))
		assert.Equal(t, "invalid", result)
	})

	t.Run("unmarshals boolean", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`true`)))
		assert.Equal(t, true, result)
	})

	t.Run("unmarshals null", func(t *testing.T) {
		result := config.UnmarshalJSONB(datatypes.JSON([]byte(`null`)))
		assert.Nil(t, result)
	})
}

func TestPaginateItems(t *testing.T) {
	t.Run("returns correct page", func(t *testing.T) {
		items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

		result := paginateItems(items, 1, 3)
		assert.Equal(t, []int{1, 2, 3}, result)

		result = paginateItems(items, 2, 3)
		assert.Equal(t, []int{4, 5, 6}, result)

		result = paginateItems(items, 4, 3)
		assert.Equal(t, []int{10}, result)
	})

	t.Run("returns empty for out of range page", func(t *testing.T) {
		items := []int{1, 2, 3}

		result := paginateItems(items, 999, 10)
		assert.Len(t, result, 0)
	})

	t.Run("normalizes invalid page size", func(t *testing.T) {
		items := []int{1, 2, 3, 4, 5}

		// Page size 0 should default to 20
		result := paginateItems(items, 1, 0)
		assert.Equal(t, items, result)

		// Page size > 100 should default to 20
		result = paginateItems(items, 1, 101)
		assert.Equal(t, items, result)
	})

	t.Run("normalizes invalid page number", func(t *testing.T) {
		items := []int{1, 2, 3}

		// Page 0 should be treated as page 1
		result := paginateItems(items, 0, 10)
		assert.Equal(t, items, result)

		// Negative page should be treated as page 1
		result = paginateItems(items, -1, 10)
		assert.Equal(t, items, result)
	})

	t.Run("handles empty slice", func(t *testing.T) {
		var items []int

		result := paginateItems(items, 1, 10)
		assert.Len(t, result, 0)
	})

	t.Run("handles exact page boundary", func(t *testing.T) {
		items := []int{1, 2, 3, 4, 5, 6}

		result := paginateItems(items, 2, 3)
		assert.Equal(t, []int{4, 5, 6}, result)
	})
}

func TestMarshalAndValidateValue(t *testing.T) {
	t.Run("validates string type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.string",
			ValueType: config.ValueTypeString,
		}

		result, err := marshalAndValidateValue("hello", entry)
		require.NoError(t, err)
		assert.Equal(t, `"hello"`, string(result))

		_, err = marshalAndValidateValue(123, entry)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expected string")
	})

	t.Run("validates int type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.int",
			ValueType: config.ValueTypeInt,
		}

		result, err := marshalAndValidateValue(42, entry)
		require.NoError(t, err)
		assert.Equal(t, `42`, string(result))

		// float64 with decimal part should be rejected to prevent silent truncation
		_, err = marshalAndValidateValue(42.5, entry)
		assert.Error(t, err)

		// float64 without decimal part should succeed (e.g., from JSON unmarshaling)
		result, err = marshalAndValidateValue(42.0, entry)
		require.NoError(t, err)
		assert.Equal(t, `42`, string(result))

		_, err = marshalAndValidateValue("not a number", entry)
		assert.Error(t, err)
	})

	t.Run("validates float type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.float",
			ValueType: config.ValueTypeFloat,
		}

		result, err := marshalAndValidateValue(3.14, entry)
		require.NoError(t, err)
		assert.Equal(t, `3.14`, string(result))

		result, err = marshalAndValidateValue(42, entry)
		require.NoError(t, err)
		assert.Equal(t, `42`, string(result))
	})

	t.Run("validates bool type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.bool",
			ValueType: config.ValueTypeBool,
		}

		result, err := marshalAndValidateValue(true, entry)
		require.NoError(t, err)
		assert.Equal(t, `true`, string(result))

		_, err = marshalAndValidateValue("true", entry)
		assert.Error(t, err)
	})

	t.Run("validates duration type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.duration",
			ValueType: config.ValueTypeDuration,
		}

		result, err := marshalAndValidateValue("5m", entry)
		require.NoError(t, err)
		assert.Equal(t, `"5m"`, string(result))

		_, err = marshalAndValidateValue("invalid", entry)
		assert.Error(t, err)
	})

	t.Run("validates json type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.json",
			ValueType: config.ValueTypeJSON,
		}

		data := map[string]interface{}{"key": "value"}
		result, err := marshalAndValidateValue(data, entry)
		require.NoError(t, err)

		var unmarshaled map[string]interface{}
		err = json.Unmarshal(result, &unmarshaled)
		require.NoError(t, err)
		assert.Equal(t, "value", unmarshaled["key"])
	})

	t.Run("runs custom validator", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.validated",
			ValueType: config.ValueTypeInt,
			Validator: func(v interface{}) error {
				if n, ok := v.(int); ok && n < 0 {
					return ErrValidation
				}
				return nil
			},
		}

		_, err := marshalAndValidateValue(-1, entry)
		assert.Error(t, err)

		_, err = marshalAndValidateValue(10, entry)
		assert.NoError(t, err)
	})

	t.Run("handles unknown value type", func(t *testing.T) {
		entry := &config.ConfigEntry{
			Key:       "test.unknown",
			ValueType: "unknown_type",
		}

		_, err := marshalAndValidateValue("value", entry)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown value_type")
	})
}

func TestConvertValueByType(t *testing.T) {
	t.Run("converts string", func(t *testing.T) {
		result, err := convertValueByType("hello", config.ValueTypeString)
		require.NoError(t, err)
		assert.Equal(t, "hello", result)

		_, err = convertValueByType(123, config.ValueTypeString)
		assert.Error(t, err)
	})

	t.Run("converts int from various types", func(t *testing.T) {
		// From int
		result, err := convertValueByType(42, config.ValueTypeInt)
		require.NoError(t, err)
		assert.Equal(t, 42, result)

		// From int64
		result, err = convertValueByType(int64(42), config.ValueTypeInt)
		require.NoError(t, err)
		assert.Equal(t, 42, result)

		// From float64 with decimal part → rejected to prevent silent truncation
		_, err = convertValueByType(42.5, config.ValueTypeInt)
		assert.Error(t, err)

		// From float64 without decimal part → accepted
		result, err = convertValueByType(42.0, config.ValueTypeInt)
		require.NoError(t, err)
		assert.Equal(t, 42, result)

		// From json.Number
		result, err = convertValueByType(json.Number("42"), config.ValueTypeInt)
		require.NoError(t, err)
		assert.Equal(t, 42, result)

		// Invalid type
		_, err = convertValueByType("not a number", config.ValueTypeInt)
		assert.Error(t, err)
	})

	t.Run("converts float from various types", func(t *testing.T) {
		// From float64
		result, err := convertValueByType(3.14, config.ValueTypeFloat)
		require.NoError(t, err)
		assert.Equal(t, 3.14, result)

		// From int
		result, err = convertValueByType(42, config.ValueTypeFloat)
		require.NoError(t, err)
		assert.Equal(t, float64(42), result)

		// From int64
		result, err = convertValueByType(int64(42), config.ValueTypeFloat)
		require.NoError(t, err)
		assert.Equal(t, float64(42), result)

		// From json.Number
		result, err = convertValueByType(json.Number("3.14"), config.ValueTypeFloat)
		require.NoError(t, err)
		assert.Equal(t, 3.14, result)

		// Invalid type
		_, err = convertValueByType("not a number", config.ValueTypeFloat)
		assert.Error(t, err)
	})

	t.Run("converts bool", func(t *testing.T) {
		result, err := convertValueByType(true, config.ValueTypeBool)
		require.NoError(t, err)
		assert.Equal(t, true, result)

		_, err = convertValueByType("true", config.ValueTypeBool)
		assert.Error(t, err)
	})

	t.Run("converts duration", func(t *testing.T) {
		result, err := convertValueByType("5m", config.ValueTypeDuration)
		require.NoError(t, err)
		assert.Equal(t, "5m", result)

		_, err = convertValueByType("invalid", config.ValueTypeDuration)
		assert.Error(t, err)

		_, err = convertValueByType(123, config.ValueTypeDuration)
		assert.Error(t, err)
	})

	t.Run("converts json", func(t *testing.T) {
		// Valid JSON object
		data := map[string]interface{}{"key": "value"}
		result, err := convertValueByType(data, config.ValueTypeJSON)
		require.NoError(t, err)
		assert.Equal(t, data, result)

		// Valid JSON array
		arrayData := []interface{}{"item1", "item2"}
		result, err = convertValueByType(arrayData, config.ValueTypeJSON)
		require.NoError(t, err)
		assert.Equal(t, arrayData, result)

		// Invalid: null value
		_, err = convertValueByType(nil, config.ValueTypeJSON)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be null")

		// Invalid: string primitive
		_, err = convertValueByType("not a json structure", config.ValueTypeJSON)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be an object or array")

		// Invalid: number primitive
		_, err = convertValueByType(123, config.ValueTypeJSON)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be an object or array")

		// Invalid: bool primitive
		_, err = convertValueByType(true, config.ValueTypeJSON)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be an object or array")
	})

	t.Run("handles unknown type", func(t *testing.T) {
		_, err := convertValueByType("value", "unknown")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown value_type")
	})
}

func TestUserIDString(t *testing.T) {
	t.Run("returns nil for nil UUID", func(t *testing.T) {
		result := userIDString(nil)
		assert.Nil(t, result)
	})

	t.Run("returns string for non-nil UUID", func(t *testing.T) {
		uid := uuid.Must(uuid.NewV7())
		result := userIDString(&uid)
		assert.Equal(t, uid.String(), result)
	})
}
