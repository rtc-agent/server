# Prompt Templates

Centralized prompt management for the RTC-Agent LLM system.

## Directory Structure

```
prompts/
├── system/              # System prompt sections (9 files)
│   ├── identity.md
│   ├── first-step.md
│   ├── workflow.md
│   ├── doing-tasks.md
│   ├── using-your-tools.md
│   ├── critical-rules.md
│   ├── language-and-principles.md
│   ├── memory-system.md
│   └── todo-system.md
├── tools/               # Tool descriptions (28 files)
│   ├── ls.md, read.md, write.md, grep.md, find.md, script.md
│   ├── ask-user.md, todo-write.md, sub-agent.md, ...
│   └── create-goal.md, complete-goal.md, ...
├── outputs/             # Output message templates (25 files)
│   ├── tool-error.md.tmpl, tool-timeout.md.tmpl, ...
│   ├── session-memory-saved.md.tmpl, ...
│   └── no-session-memories.md, ...
├── attachments/         # Context attachment templates (6 files)
│   ├── todo-list.md.tmpl
│   ├── session-memory-injection.md.tmpl
│   ├── session-memory-summary.md.tmpl
│   ├── user-memory-wrapper.md.tmpl
│   ├── user-memory-preamble-en.md
│   └── user-memory-preamble-zh.md
├── goal-creation.md     # Goal workflow creation prompt
├── goal-management.md.tmpl  # Goal management (dynamic)
├── loop-creation.md     # Loop workflow creation prompt
├── loop-management.md.tmpl  # Loop management (dynamic)
├── persona-trigger.md.tmpl  # Persona command trigger
├── persona-sustain.md.tmpl  # Persona command sustain
├── base-compact-prompt.md   # Full context compression
├── partial-compact-prompt.md    # Partial compression
├── partial-compact-up-to-prompt.md  # Partial-up-to compression
├── compact-user-summary-message.md  # User-facing summary wrapper
├── no-tools-preamble.md    # Suppress tool calls during compression
├── no-tools-trailer.md     # Trailer reminder for compression
├── session-memory-extract.md   # Session memory extraction instructions
└── session-title-summarize.md  # Session title generation instructions
```

## Naming Convention

| Extension | Meaning | Rendering |
|-----------|---------|-----------|
| `.md` | Static text, no template variables | Used as-is via `renderStatic()` or direct variable reference |
| `.md.tmpl` | Contains `{{.Field}}` placeholders | Rendered via `mustRenderTemplate()` |

## Go Manager Files

Each category has a corresponding Go file in `server/internal/agent/`:

| File | Responsibility |
|------|---------------|
| `system_prompts.go` | System prompt section composition via `SystemPromptBuilder` |
| `tool_prompts.go` | Tool description embedding (28 tools) |
| `output_prompts.go` | Output message formatting (tool errors, memory confirmations, etc.) |
| `attachment_prompts.go` | Context attachment formatting (todo list, memories) |
| `workflow_prompts.go` | Goal/Loop workflow prompt rendering |
| `compact_prompt.go` | Context compression prompt assembly |
| `builtin_commands.go` | Persona command template embedding |
| `session_memory_extractor.go` | Session memory extraction prompt |
| `session_title_summarizer.go` | Title generation prompt |

## How to Add a New Prompt

### Adding a tool description

1. Create `prompts/tools/<tool-name>.md`
2. Add embed in `tool_prompts.go`:
   ```go
   //go:embed prompts/tools/<tool-name>.md
   var <toolName>Desc string
   ```
3. Reference `<toolName>Desc` in your tool registration code

### Adding an output template

1. Create `prompts/outputs/<name>.md` (static) or `<name>.md.tmpl` (dynamic)
2. Add embed in `output_prompts.go`:
   ```go
   //go:embed prompts/outputs/<name>.md.tmpl
   var <name>Tmpl string
   ```
3. Add a `format<Name>(args...) string` accessor that calls `mustRenderTemplate`

### Adding a system prompt section

1. Create `prompts/system/<section>.md`
2. Add embed in `system_prompts.go`:
   ```go
   //go:embed prompts/system/<section>.md
   var systemPrompt<Section> string
   ```
3. Add to `defaultSystemPromptSections` slice in desired position

## Template Syntax

Templates use Go's `text/template` package.

### Common patterns

```
{{.FieldName}}              — Access a struct field or map key
{{range .Items}}...{{end}}  — Iterate over a slice
{{if .Field}}...{{end}}     — Conditional rendering
```

### Error handling

- `mustRenderTemplate()` panics on error (embedded templates are compile-time verified)
- `SystemPromptBuilder.Build()` returns an error (sections may be user-provided)
- `BuildOrDefault()` returns a minimal fallback on error
