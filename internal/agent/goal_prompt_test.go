package agent

import (
	"strings"
	"testing"

	"github.com/rtc-agent/server/internal/model"
)

func TestBuildGoalManagementPrompt(t *testing.T) {
	goal := &model.Goal{
		Condition:      "All 42 tests pass",
		CompletedTurns: 2,
		MaxTurns:       50,
	}

	prompt := buildGoalManagementPrompt(goal)

	// Check that prompt contains key information
	if prompt == "" {
		t.Error("prompt should not be empty")
	}

	if !strings.Contains(prompt, "All 42 tests pass") {
		t.Error("prompt should contain goal condition")
	}

	if !strings.Contains(prompt, "Turn 2/50") {
		t.Error("prompt should contain progress information")
	}

	if !strings.Contains(prompt, "complete_goal") {
		t.Error("prompt should mention complete_goal tool")
	}
	if !strings.Contains(prompt, "cancel_goal") {
		t.Error("prompt should mention cancel_goal tool")
	}
}
