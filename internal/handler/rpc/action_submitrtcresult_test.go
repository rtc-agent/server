package rpchandler

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONCompact_PreservesKeyOrder(t *testing.T) {
	// 测试 json.Compact 是否保留 JSON key 的原始顺序
	input := `{"logs": ["hello"], "duration_ms": 42, "exit_code": 0}`

	var buf bytes.Buffer
	err := json.Compact(&buf, []byte(input))
	require.NoError(t, err)

	result := buf.String()

	// json.Compact 应该只移除空格，不改变 key 顺序
	// 预期结果：紧凑格式，但 key 顺序保持 logs -> duration_ms -> exit_code
	assert.Equal(t, `{"logs":["hello"],"duration_ms":42,"exit_code":0}`, result)
}

func TestJSONCompact_RemovesSpaces(t *testing.T) {
	// 测试 json.Compact 移除空格
	input := `{
		"file": "/functions/INDEX.md",
		"line": "| delete | Delete a task |",
		"lineNumber": 21
	}`

	var buf bytes.Buffer
	err := json.Compact(&buf, []byte(input))
	require.NoError(t, err)

	result := buf.String()

	// 应该移除所有多余空格
	assert.Equal(t, `{"file":"/functions/INDEX.md","line":"| delete | Delete a task |","lineNumber":21}`, result)
}

func TestJSONCompact_ComplexJSON(t *testing.T) {
	// 测试复杂 JSON 的 compact 处理
	input := `[
		{"file": "/functions/INDEX.md", "line": "| delete | Delete a task |", "lineNumber": 21},
		{"file": "/functions/task/delete.md", "line": "# task.delete", "lineNumber": 3}
	]`

	var buf bytes.Buffer
	err := json.Compact(&buf, []byte(input))
	require.NoError(t, err)

	result := buf.String()

	// 应该保留数组结构和 key 顺序
	expected := `[{"file":"/functions/INDEX.md","line":"| delete | Delete a task |","lineNumber":21},{"file":"/functions/task/delete.md","line":"# task.delete","lineNumber":3}]`
	assert.Equal(t, expected, result)
}
