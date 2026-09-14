package agent

import (
	"strings"
	"testing"
)

func TestBuildDefaultSystemPrompt(t *testing.T) {
	prompt, err := BuildDefaultSystemPrompt()
	if err != nil {
		t.Fatalf("BuildDefaultSystemPrompt() error: %v", err)
	}
	if prompt == "" {
		t.Fatal("BuildDefaultSystemPrompt() returned empty string")
	}

	// Verify all key sections are present.
	required := []string{
		"capable AI assistant",
		"MANDATORY FIRST STEP",
		"AGENT.md",
		"Workflow after reading AGENT.md",
		"Doing tasks",
		"Read before acting",
		"Verify before claiming completion",
		"Diagnose before switching",
		"Using your tools",
		"Tool Preference",
		"Parallel Tool Calls",
		"CRITICAL RULES",
		"Language:",
		"Principles:",
		"Memory System:",
		"Session Memory",
		"User Memory",
		"Todo List",
		"todo_write",
	}
	for _, substr := range required {
		if !strings.Contains(prompt, substr) {
			t.Errorf("default system prompt missing %q", substr)
		}
	}
}

func TestSystemPromptBuilder_WithData(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.WithData("key", "value")
	if builder.data["key"] != "value" {
		t.Error("WithData did not store value")
	}
}

func TestSystemPromptBuilder_WithDataMap(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.WithDataMap(map[string]any{
		"a": 1,
		"b": "two",
	})
	if builder.data["a"] != 1 || builder.data["b"] != "two" {
		t.Error("WithDataMap did not store values")
	}
}

func TestSystemPromptBuilder_AddSection(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.AddSection("CUSTOM SECTION CONTENT")
	prompt := builder.BuildOrDefault()
	if !strings.Contains(prompt, "CUSTOM SECTION CONTENT") {
		t.Error("AddSection did not append custom section")
	}
}

func TestSystemPromptBuilder_PrependSection(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.PrependSection("FIRST SECTION")
	prompt := builder.BuildOrDefault()
	if !strings.HasPrefix(prompt, "FIRST SECTION") {
		t.Error("PrependSection did not prepend custom section")
	}
}

func TestSystemPromptBuilder_ResetSections(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.ResetSections("ONLY THIS")
	prompt := builder.BuildOrDefault()
	if prompt != "ONLY THIS" {
		t.Errorf("ResetSections expected 'ONLY THIS', got %q", prompt)
	}
}

func TestSystemPromptBuilder_TemplateRendering(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.ResetSections("Hello {{.Name}}, you are {{.Role}}.")
	builder.WithData("Name", "Alice")
	builder.WithData("Role", "an admin")
	prompt, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	expected := "Hello Alice, you are an admin."
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestSystemPromptBuilder_EmptySectionsSkipped(t *testing.T) {
	builder := NewSystemPromptBuilder()
	builder.ResetSections("first", "  ", "", "last")
	prompt := builder.BuildOrDefault()
	// Empty/whitespace-only sections should be skipped, no double separators.
	if prompt != "first\n\nlast" {
		t.Errorf("expected 'first\\n\\nlast', got %q", prompt)
	}
}

func TestBuildOrDefault_FallbackOnTemplateError(t *testing.T) {
	builder := NewSystemPromptBuilder()
	// Invalid template syntax.
	builder.ResetSections("{{.Missing | bad}}")
	prompt := builder.BuildOrDefault()
	if !strings.Contains(prompt, "helpful AI assistant") {
		t.Errorf("BuildOrDefault did not return fallback on error, got %q", prompt)
	}
}

func TestSystemPromptBuilder_Chaining(t *testing.T) {
	prompt, err := NewSystemPromptBuilder().
		WithData("k", "v").
		AddSection("extra").
		Build()
	if err != nil {
		t.Fatalf("chained Build() error: %v", err)
	}
	if !strings.Contains(prompt, "extra") {
		t.Error("chained AddSection not reflected in output")
	}
}

func TestDefaultSystemPrompt_MatchesOriginal(t *testing.T) {
	// Verify the assembled prompt is equivalent to the original monolithic YAML prompt.
	// This is the backward-compatibility guarantee.
	prompt, err := BuildDefaultSystemPrompt()
	if err != nil {
		t.Fatalf("BuildDefaultSystemPrompt() error: %v", err)
	}

	// Key phrases from the prompt that must be preserved.
	// These span all sections including the new doing-tasks and using-your-tools sections.
	originalPhrases := []string{
		"capable AI assistant",
		"When you receive ANY message from the user, you MUST immediately call the read tool to read AGENT.md",
		"functions/INDEX.md",
		"scenarios/INDEX.md",
		"scripts/ (optional)",
		"NEVER pretend to execute a task",
		"If a tool call fails due to parameter errors",
		"Read before acting",
		"Verify before claiming completion",
		"Diagnose before switching",
		"Don't over-engineer",
		"Tool Preference",
		"Parallel Tool Calls",
		"Detect the language used by the user",
		"ALWAYS use tools to complete tasks",
		"save_session_memory tool",
		"save_user_memory tool",
		"search_memory",
		"todo_write to update the entire todo list",
		"in_progress status at all times",
	}
	for _, phrase := range originalPhrases {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("backward compatibility broken: missing original phrase %q", phrase)
		}
	}
}
