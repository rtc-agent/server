package agent

import (
	"testing"

	"github.com/rtc-agent/server/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestCompactableToolsIncludesTodoWrite(t *testing.T) {
	_, ok := CompactableTools["todoWrite"]
	assert.True(t, ok, "CompactableTools should include todoWrite")
}

func TestFormatTodoList_ToolResultFormat(t *testing.T) {
	todos := []model.TodoItem{
		{Content: "Implement login", ActiveForm: "Implementing login", Status: "in_progress"},
		{Content: "Write tests", ActiveForm: "Writing tests", Status: "pending"},
		{Content: "Design schema", ActiveForm: "Designing schema", Status: "completed"},
	}
	result := formatTodoList(todos)
	assert.Contains(t, result, "## Current Tasks")
	assert.Contains(t, result, "In Progress")
	assert.Contains(t, result, "**Implement login**")
	assert.Contains(t, result, "Pending")
	assert.Contains(t, result, "Write tests")
	assert.Contains(t, result, "Completed")
	assert.Contains(t, result, "~~Design schema~~")
}

func TestFormatTodoList_EmptyList(t *testing.T) {
	result := formatTodoList(nil)
	// Empty list should still produce valid output
	assert.NotNil(t, result)
}
