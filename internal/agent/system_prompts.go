// system_prompts.go — Composable system prompt sections.
//
// This implements a section-based system prompt architecture inspired by
// Claude Code's systemPromptSection design. Each section is an independent
// Markdown file that can be composed, overridden, or extended.
//
// The default system prompt is assembled from these sections in order:
//  1. identity.md              — Agent identity, capabilities, and how you work
//  2. first-step.md            — Mandatory first step (read AGENT.md)
//  3. workflow.md              — Workflow after reading AGENT.md
//  4. doing-tasks.md           — Task execution principles (verify, diagnose, don't over-engineer)
//  5. using-your-tools.md     — Tool preference hierarchy and parallel call guidance
//  6. critical-rules.md        — Critical tool usage rules
//  7. language-and-principles.md — Language detection and principles
//  8. memory-system.md         — Session and User memory system
//  9. todo-system.md           — Todo list behavior
//
// Configuration override: If worker.system_prompt is set in config YAML,
// it completely replaces the default embedded prompt. This preserves backward
// compatibility and allows full customization without code changes.
//
// Naming convention:
//   - Variables: systemPrompt<Section> (e.g. systemPromptIdentity)
//   - Files: prompts/system/<section-name>.md (all static, template-ready)
//   - Builder: SystemPromptBuilder (fluent API for composing sections)
package agent

import (
	_ "embed"
	"strings"

	"github.com/rtc-agent/server/internal/agent/templateutil"
)

//go:embed prompts/system/identity.md
var systemPromptIdentity string

//go:embed prompts/system/first-step.md
var systemPromptFirstStep string

//go:embed prompts/system/workflow.md
var systemPromptWorkflow string

//go:embed prompts/system/doing-tasks.md
var systemPromptDoingTasks string

//go:embed prompts/system/using-your-tools.md
var systemPromptUsingYourTools string

//go:embed prompts/system/critical-rules.md
var systemPromptCriticalRules string

//go:embed prompts/system/language-and-principles.md
var systemPromptLanguageAndPrinciples string

//go:embed prompts/system/memory-system.md
var systemPromptMemorySystem string

//go:embed prompts/system/todo-system.md
var systemPromptTodoSystem string

// defaultSystemPromptSections defines the default order of system prompt sections.
var defaultSystemPromptSections = []string{
	systemPromptIdentity,
	systemPromptFirstStep,
	systemPromptWorkflow,
	systemPromptDoingTasks,
	systemPromptUsingYourTools,
	systemPromptCriticalRules,
	systemPromptLanguageAndPrinciples,
	systemPromptMemorySystem,
	systemPromptTodoSystem,
}

// SystemPromptBuilder builds the complete system prompt from composable sections.
//
// Sections are rendered as Go text/template with the provided data map, then
// concatenated with double-newline separators. This allows sections to
// reference dynamic values (e.g., {{.AgentName}}) in the future.
type SystemPromptBuilder struct {
	sections []string
	data     map[string]any
}

// NewSystemPromptBuilder creates a builder pre-loaded with the default sections.
func NewSystemPromptBuilder() *SystemPromptBuilder {
	return &SystemPromptBuilder{
		sections: append([]string{}, defaultSystemPromptSections...),
		data:     make(map[string]any),
	}
}

// WithData adds a template data key-value pair.
func (b *SystemPromptBuilder) WithData(key string, value any) *SystemPromptBuilder {
	b.data[key] = value
	return b
}

// WithDataMap merges an entire data map into the builder.
func (b *SystemPromptBuilder) WithDataMap(data map[string]any) *SystemPromptBuilder {
	for k, v := range data {
		b.data[k] = v
	}
	return b
}

// ResetSections replaces all sections with the given list.
// Useful for config overrides that provide a complete replacement prompt.
func (b *SystemPromptBuilder) ResetSections(sections ...string) *SystemPromptBuilder {
	b.sections = sections
	return b
}

// AddSection appends a custom section after the default sections.
func (b *SystemPromptBuilder) AddSection(section string) *SystemPromptBuilder {
	b.sections = append(b.sections, section)
	return b
}

// PrependSection inserts a custom section before all existing sections.
func (b *SystemPromptBuilder) PrependSection(section string) *SystemPromptBuilder {
	b.sections = append([]string{section}, b.sections...)
	return b
}

// Build renders all sections as templates and concatenates them.
// Returns an error if any section fails to parse or execute as a template.
func (b *SystemPromptBuilder) Build() (string, error) {
	var parts []string
	for _, section := range b.sections {
		rendered, err := templateutil.Render("section", section, b.data)
		if err != nil {
			return "", err
		}
		trimmed := strings.TrimSpace(rendered)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// BuildOrDefault builds the system prompt, returning a minimal fallback on error.
func (b *SystemPromptBuilder) BuildOrDefault() string {
	result, err := b.Build()
	if err != nil {
		return "You are a helpful AI assistant. Detect the user's language and respond in that language."
	}
	return result
}

// BuildDefaultSystemPrompt assembles the default system prompt with no template data.
// This is the canonical default used when worker.system_prompt is not configured.
func BuildDefaultSystemPrompt() (string, error) {
	return NewSystemPromptBuilder().Build()
}
