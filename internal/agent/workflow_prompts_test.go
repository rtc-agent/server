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

	if prompt == "" {
		t.Fatal("prompt should not be empty")
	}

	tests := []struct {
		name string
		want string
	}{
		{"condition", "All 42 tests pass"},
		{"progress", "Turn 2/50"},
		{"complete_tool", "completeGoal"},
		{"cancel_tool", "cancelGoal"},
		{"heading", "# Goal Management"},
	}
	for _, tt := range tests {
		if !strings.Contains(prompt, tt.want) {
			t.Errorf("prompt missing %s: want %q", tt.name, tt.want)
		}
	}
}

func TestBuildLoopManagementPrompt(t *testing.T) {
	loop := &model.Loop{
		Prompt:          "Check deployment health",
		CompletedTurns:  3,
		MaxTurns:        10,
		IntervalSeconds: 60,
	}

	prompt := buildLoopManagementPrompt(loop)

	if prompt == "" {
		t.Fatal("prompt should not be empty")
	}

	tests := []struct {
		name string
		want string
	}{
		{"prompt_text", "Check deployment health"},
		{"progress", "Turn 3/10"},
		{"interval", "60 seconds"},
		{"complete_tool", "completeLoop"},
		{"cancel_tool", "cancelLoop"},
		{"heading", "# Loop Management"},
	}
	for _, tt := range tests {
		if !strings.Contains(prompt, tt.want) {
			t.Errorf("prompt missing %s: want %q", tt.name, tt.want)
		}
	}
}

func TestGoalCreationPromptEmbedded(t *testing.T) {
	if goalCreationPrompt == "" {
		t.Fatal("goalCreationPrompt should not be empty")
	}
	if !strings.Contains(goalCreationPrompt, "# Goal Creation") {
		t.Error("goalCreationPrompt missing heading")
	}
	if !strings.Contains(goalCreationPrompt, "createGoal") {
		t.Error("goalCreationPrompt missing createGoal reference")
	}
}

func TestLoopCreationPromptEmbedded(t *testing.T) {
	if loopCreationPrompt == "" {
		t.Fatal("loopCreationPrompt should not be empty")
	}
	if !strings.Contains(loopCreationPrompt, "# Loop Creation") {
		t.Error("loopCreationPrompt missing heading")
	}
	if !strings.Contains(loopCreationPrompt, "createLoop") {
		t.Error("loopCreationPrompt missing createLoop reference")
	}
}

func TestPersonaTriggerTemplateEmbedded(t *testing.T) {
	if personaTriggerTmpl == "" {
		t.Fatal("personaTriggerTmpl should not be empty")
	}
	if !strings.Contains(personaTriggerTmpl, "{{.Args}}") {
		t.Error("personaTriggerTmpl missing {{.Args}} placeholder")
	}
}

func TestPersonaSustainTemplateEmbedded(t *testing.T) {
	if personaSustainTmpl == "" {
		t.Fatal("personaSustainTmpl should not be empty")
	}
	if !strings.Contains(personaSustainTmpl, "{{.Args}}") {
		t.Error("personaSustainTmpl missing {{.Args}} placeholder")
	}
}
