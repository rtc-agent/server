package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── mustMarshalJSON Tests ───

func TestMustMarshalJSON_Map(t *testing.T) {
	t.Parallel()

	result, err := mustMarshalJSON(map[string]string{"key": "value"})
	require.NoError(t, err)
	assert.Equal(t, `{"key":"value"}`, result)
}

func TestMustMarshalJSON_Struct(t *testing.T) {
	t.Parallel()

	type testStruct struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	result, err := mustMarshalJSON(testStruct{Name: "test", Count: 42})
	require.NoError(t, err)
	assert.Equal(t, `{"name":"test","count":42}`, result)
}

func TestMustMarshalJSON_Slice(t *testing.T) {
	t.Parallel()

	result, err := mustMarshalJSON([]int{1, 2, 3})
	require.NoError(t, err)
	assert.Equal(t, `[1,2,3]`, result)
}

func TestMustMarshalJSON_String(t *testing.T) {
	t.Parallel()

	result, err := mustMarshalJSON("hello")
	require.NoError(t, err)
	assert.Equal(t, `"hello"`, result)
}

func TestMustMarshalJSON_Nil(t *testing.T) {
	t.Parallel()

	result, err := mustMarshalJSON(nil)
	require.NoError(t, err)
	assert.Equal(t, `null`, result)
}

func TestMustMarshalJSON_NestedStruct(t *testing.T) {
	t.Parallel()

	type nested struct {
		Inner string `json:"inner"`
	}
	type outer struct {
		Data  nested `json:"data"`
		Value int    `json:"value"`
	}

	result, err := mustMarshalJSON(outer{Data: nested{Inner: "deep"}, Value: 100})
	require.NoError(t, err)
	assert.Contains(t, result, `"data"`)
	assert.Contains(t, result, `"inner":"deep"`)
	assert.Contains(t, result, `"value":100`)
}

func TestMustMarshalJSON_Unmarshalable(t *testing.T) {
	t.Parallel()

	// Channels cannot be marshalled to JSON
	ch := make(chan int)
	_, err := mustMarshalJSON(ch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal json")
}

func TestMustMarshalJSON_EmptyMap(t *testing.T) {
	t.Parallel()

	result, err := mustMarshalJSON(map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, `{}`, result)
}

func TestMustMarshalJSON_Bool(t *testing.T) {
	t.Parallel()

	result, err := mustMarshalJSON(true)
	require.NoError(t, err)
	assert.Equal(t, `true`, result)

	result2, err2 := mustMarshalJSON(false)
	require.NoError(t, err2)
	assert.Equal(t, `false`, result2)
}

// ─── publishToolMessages Validation Tests ───
//
// Note: Full happy-path testing requires eino's internal tool call context
// which cannot be injected from outside the package. These tests cover
// the validation error paths.

func TestPublishToolMessages_MissingToolCallID(t *testing.T) {
	t.Parallel()

	// No tool_call_id set in context — should return error
	ctx := context.Background()
	in := publishToolMessagesInput{
		Helpers:    &helpers{},
		SessionID:  uuid.New(),
		TurnID:     uuid.New(),
		ToolName:   "test_tool",
		ResultJSON: `{}`,
	}

	err := publishToolMessages(ctx, in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call_id not set in context")
	assert.Contains(t, err.Error(), "test_tool")
}

func TestPublishToolMessages_NilTurnID(t *testing.T) {
	t.Parallel()

	// We can't set tool_call_id from outside eino, so we test nil TurnID
	// which is checked BEFORE the callID check in the validation order...
	// Actually, looking at the code, callID is checked first.
	// Without the tool_call_id, we'll get that error first.
	// This test documents that behavior.
	ctx := context.Background()
	in := publishToolMessagesInput{
		Helpers:    &helpers{},
		SessionID:  uuid.New(),
		TurnID:     uuid.Nil, // nil turn ID
		ToolName:   "test_tool",
		ResultJSON: `{}`,
	}

	err := publishToolMessages(ctx, in)
	require.Error(t, err)
	// callID check happens first, so we get that error
	assert.Contains(t, err.Error(), "tool_call_id not set in context")
}

func TestPublishOutputOnly_MissingToolCallID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	in := publishOutputOnlyInput{
		Helpers:         &helpers{},
		SessionID:       uuid.New(),
		TurnID:          uuid.New(),
		ToolName:        "async_tool",
		ArgumentsInJSON: `{}`,
		Output:          `{"result": "done"}`,
		ParentMessageID: uuid.New(),
	}

	err := publishOutputOnly(ctx, in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call_id not set in context")
	assert.Contains(t, err.Error(), "async_tool")
}
