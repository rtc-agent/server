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
		"Doing tasks",
		"Read before acting",
		"Verify before claiming completion",
		"Diagnose before switching",
		"Don't over-engineer",
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
		"todoWrite",
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

func TestDefaultSystemPrompt_NewStructure(t *testing.T) {
	// Verify the system prompt structure after refactoring.
	// Note: Agent identity/workflow is now in the agent prompt (agent_prompts.go),
	// not in the system prompt.
	prompt, err := BuildDefaultSystemPrompt()
	if err != nil {
		t.Fatalf("BuildDefaultSystemPrompt() error: %v", err)
	}

	// Key phrases from the system prompt sections.
	requiredPhrases := []string{
		"Doing tasks",
		"Read before acting",
		"Verify before claiming completion",
		"Tool Preference",
		"CRITICAL RULES",
		"Language:",
		"Memory System:",
		"Todo List",
	}
	for _, phrase := range requiredPhrases {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("system prompt structure broken: missing phrase %q", phrase)
		}
	}
}
