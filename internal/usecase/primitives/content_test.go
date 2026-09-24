package primitives

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseContentDataToolCallRaw_PreservesKeyOrder(t *testing.T) {
	// 测试用例：JSON key 顺序必须保留，这对 LLM 缓存命中至关重要
	contentJSON := `{
		"type": "toolcall_output",
		"data": {
			"id": "tool-call-123",
			"tool_name": "script",
			"output": {"logs": ["hello"], "duration_ms": 42, "exit_code": 0},
			"status": "completed"
		}
	}`

	toolCall, err := ParseContentDataToolCallRaw(contentJSON)
	require.NoError(t, err)

	// 验证基本字段
	assert.Equal(t, "tool-call-123", toolCall.Id)
	assert.Equal(t, "script", toolCall.ToolName)
	require.NotNil(t, toolCall.Output)

	// 关键验证：JSON key 顺序必须保留
	// 原始顺序：logs, duration_ms, exit_code
	// 如果经过 map[string]interface{}，会变成字母序：duration_ms, exit_code, logs
	outputStr := *toolCall.Output

	// 验证 logs 在 duration_ms 之前
	logsIdx := strings.Index(outputStr, `"logs"`)
	durationIdx := strings.Index(outputStr, `"duration_ms"`)
	exitCodeIdx := strings.Index(outputStr, `"exit_code"`)

	assert.Greater(t, logsIdx, -1, "logs should be present")
	assert.Greater(t, durationIdx, -1, "duration_ms should be present")
	assert.Greater(t, exitCodeIdx, -1, "exit_code should be present")

	assert.Less(t, logsIdx, durationIdx, "logs should come before duration_ms (original order)")
	assert.Less(t, durationIdx, exitCodeIdx, "duration_ms should come before exit_code (original order)")
}

func TestParseContentDataToolCallRaw_BackwardCompatible(t *testing.T) {
	// 测试旧格式（input/output 为 JSON 字符串）
	oldFormatJSON := `{
		"type": "toolcall_input",
		"data": {
			"id": "tool-call-456",
			"tool_name": "read",
			"input": "{\"file\":\"test.txt\"}",
			"status": "completed"
		}
	}`

	toolCall, err := ParseContentDataToolCallRaw(oldFormatJSON)
	require.NoError(t, err)

	assert.Equal(t, "tool-call-456", toolCall.Id)
	assert.Equal(t, "read", toolCall.ToolName)
	assert.Equal(t, `{"file":"test.txt"}`, toolCall.Input)
	require.NotNil(t, toolCall.Status)
	assert.Equal(t, "completed", *toolCall.Status)
}

func TestParseContentDataToolCallRaw_NewFormat(t *testing.T) {
	// 测试新格式（input/output 为 JSON 对象）
	newFormatJSON := `{
		"type": "toolcall_input",
		"data": {
			"id": "tool-call-789",
			"tool_name": "script",
			"input": {"command": "echo hello", "timeout": 30},
			"status": "completed"
		}
	}`

	toolCall, err := ParseContentDataToolCallRaw(newFormatJSON)
	require.NoError(t, err)

	assert.Equal(t, "tool-call-789", toolCall.Id)
	assert.Equal(t, "script", toolCall.ToolName)
	// input 应该保留原始 JSON 格式
	assert.Contains(t, toolCall.Input, "command")
	assert.Contains(t, toolCall.Input, "echo hello")
}
