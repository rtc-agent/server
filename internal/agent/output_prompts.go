// output_prompts.go — Output message templates.
//
// These templates replace hardcoded output strings scattered across tool
// implementations. Centralising them here makes it easy to audit and adjust
// the text the LLM sees without touching business logic.
//
// Convention:
//   - .md        — static text, no template variables (rendered via renderStatic)
//   - .md.tmpl   — contains {{.Field}} placeholders (rendered via mustRenderTemplate)
//
// Naming convention:
//   - Variables: <name>Tmpl (e.g. toolErrorTmpl, askUserResultTmpl)
//   - Accessors: format<Name>(args...) string (e.g. formatToolError, formatAskUserResultText)
package agent

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

// --- formatToolCallOutput status templates ---

//go:embed prompts/outputs/tool-error.md.tmpl
var toolErrorTmpl string

//go:embed prompts/outputs/tool-timeout.md.tmpl
var toolTimeoutTmpl string

//go:embed prompts/outputs/tool-rejected.md.tmpl
var toolRejectedTmpl string

//go:embed prompts/outputs/tool-pending.md.tmpl
var toolPendingTmpl string

// --- Error / system templates ---

//go:embed prompts/outputs/unknown-tool.md.tmpl
var unknownToolTmpl string

//go:embed prompts/outputs/error-wrapper.md.tmpl
var errorWrapperTmpl string

//go:embed prompts/outputs/parse-error.md.tmpl
var parseErrorTmpl string

// --- Todo notification ---

//go:embed prompts/outputs/todo-notification.md
var todoNotificationTmpl string

// --- Ask user result templates ---

//go:embed prompts/outputs/ask-user-result.md.tmpl
var askUserResultTmpl string

//go:embed prompts/outputs/ask-user-no-answers.md
var askUserNoAnswersTmpl string

// --- Sub agent result templates ---

//go:embed prompts/outputs/sub-agent-async-result.md.tmpl
var subAgentAsyncResultTmpl string

//go:embed prompts/outputs/sub-agent-no-result.md
var subAgentNoResultTmpl string

//go:embed prompts/outputs/sub-agent-no-assistant.md
var subAgentNoAssistantTmpl string

//go:embed prompts/outputs/sub-agent-parse-failed.md
var subAgentParseFailedTmpl string

//go:embed prompts/outputs/sub-agent-no-output.md
var subAgentNoOutputTmpl string

// --- Session memory output templates ---

//go:embed prompts/outputs/session-memory-saved.md.tmpl
var sessionMemorySavedTmpl string

//go:embed prompts/outputs/no-session-memories.md
var noSessionMemoriesTmpl string

//go:embed prompts/outputs/session-memories-list.md.tmpl
var sessionMemoriesListTmpl string

// --- User memory output templates ---

//go:embed prompts/outputs/user-memory-saved.md.tmpl
var userMemorySavedTmpl string

//go:embed prompts/outputs/user-memory-updated.md.tmpl
var userMemoryUpdatedTmpl string

//go:embed prompts/outputs/user-memory-deleted.md.tmpl
var userMemoryDeletedTmpl string

//go:embed prompts/outputs/no-user-memories.md
var noUserMemoriesTmpl string

//go:embed prompts/outputs/user-memories-list.md.tmpl
var userMemoriesListTmpl string

// --- Search memory output templates ---

//go:embed prompts/outputs/no-search-results.md
var noSearchResultsTmpl string

//go:embed prompts/outputs/search-results-list.md.tmpl
var searchResultsListTmpl string

// =============================================================================
// Rendering helpers
// =============================================================================

// mustRenderTemplate parses and executes a text/template string with the given
// data. It panics on error because templates are embedded at compile time — a
// failure here is a programmer error that should surface immediately.
//
// This is the shared render core used by output, workflow, and attachment
// template accessors. Callers that need custom template functions (e.g. the
// attachment "trunc" helper) should use renderAttachmentTemplate instead.
func mustRenderTemplate(name, tmplStr string, data any) string {
	t, err := template.New(name).Parse(tmplStr)
	if err != nil {
		panic(fmt.Sprintf("template parse error (%s): %v", name, err))
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		panic(fmt.Sprintf("template execute error (%s): %v", name, err))
	}
	return buf.String()
}

// renderStatic returns a static template string as-is. It exists for symmetry
// with mustRenderTemplate and to make call sites self-documenting.
func renderStatic(tmplStr string) string {
	return tmplStr
}

// =============================================================================
// Type-safe accessor functions
// =============================================================================

// --- formatToolCallOutput helpers ---

func formatToolError(toolName, output string) string {
	return mustRenderTemplate("tool-error", toolErrorTmpl, map[string]any{
		"ToolName": toolName,
		"Output":   output,
	})
}

func formatToolTimeout(toolName string) string {
	return mustRenderTemplate("tool-timeout", toolTimeoutTmpl, map[string]any{
		"ToolName": toolName,
	})
}

func formatToolRejected(toolName string) string {
	return mustRenderTemplate("tool-rejected", toolRejectedTmpl, map[string]any{
		"ToolName": toolName,
	})
}

func formatToolPending(toolName, status string) string {
	return mustRenderTemplate("tool-pending", toolPendingTmpl, map[string]any{
		"ToolName": toolName,
		"Status":   status,
	})
}

// --- Error / system ---

func formatUnknownTool(toolName string) string {
	return mustRenderTemplate("unknown-tool", unknownToolTmpl, map[string]any{
		"ToolName": toolName,
	})
}

func formatErrorWrapper(errMsg string) string {
	return mustRenderTemplate("error-wrapper", errorWrapperTmpl, map[string]any{
		"Error": errMsg,
	})
}

func formatParseError(errMsg, preview string) string {
	return mustRenderTemplate("parse-error", parseErrorTmpl, map[string]any{
		"Error":   errMsg,
		"Preview": preview,
	})
}

// --- Todo ---

func formatTodoNotification() string {
	return renderStatic(todoNotificationTmpl)
}

// --- Ask user ---

func formatAskUserResultText(answers string) string {
	return mustRenderTemplate("ask-user-result", askUserResultTmpl, map[string]any{
		"Answers": answers,
	})
}

func formatAskUserNoAnswers() string {
	return renderStatic(askUserNoAnswersTmpl)
}

// --- Sub agent ---

func formatSubAgentAsyncResult(sessionID, title string) string {
	return mustRenderTemplate("sub-agent-async-result", subAgentAsyncResultTmpl, map[string]any{
		"SessionID": sessionID,
		"Title":     title,
	})
}

func formatSubAgentNoResult() string {
	return renderStatic(subAgentNoResultTmpl)
}

func formatSubAgentNoAssistant() string {
	return renderStatic(subAgentNoAssistantTmpl)
}

func formatSubAgentParseFailed() string {
	return renderStatic(subAgentParseFailedTmpl)
}

func formatSubAgentNoOutput() string {
	return renderStatic(subAgentNoOutputTmpl)
}

// --- Session memory ---

func formatSessionMemorySaved(id, category, title string) string {
	return mustRenderTemplate("session-memory-saved", sessionMemorySavedTmpl, map[string]any{
		"ID":       id,
		"Category": category,
		"Title":    title,
	})
}

func formatNoSessionMemories() string {
	return renderStatic(noSessionMemoriesTmpl)
}

type sessionMemoryItem struct {
	Index     int
	Category  string
	Title     string
	Content   string
	CreatedAt string
}

func formatSessionMemoriesList(count int, memories []sessionMemoryItem) string {
	return mustRenderTemplate("session-memories-list", sessionMemoriesListTmpl, map[string]any{
		"Count":    count,
		"Memories": memories,
	})
}

// --- User memory ---

func formatUserMemorySaved(id, category, importance, title string) string {
	return mustRenderTemplate("user-memory-saved", userMemorySavedTmpl, map[string]any{
		"ID":         id,
		"Category":   category,
		"Importance": importance,
		"Title":      title,
	})
}

func formatUserMemoryUpdated(memoryID string) string {
	return mustRenderTemplate("user-memory-updated", userMemoryUpdatedTmpl, map[string]any{
		"MemoryID": memoryID,
	})
}

func formatUserMemoryDeleted(memoryID string) string {
	return mustRenderTemplate("user-memory-deleted", userMemoryDeletedTmpl, map[string]any{
		"MemoryID": memoryID,
	})
}

func formatNoUserMemories() string {
	return renderStatic(noUserMemoriesTmpl)
}

type userMemoryItem struct {
	Index       int
	Category    string
	Importance  string
	Title       string
	Content     string
	ID          string
	CreatedAt   string
	AccessCount int
}

func formatUserMemoriesList(count int, memories []userMemoryItem) string {
	return mustRenderTemplate("user-memories-list", userMemoriesListTmpl, map[string]any{
		"Count":    count,
		"Memories": memories,
	})
}

// --- Search memory ---

func formatNoSearchResults() string {
	return renderStatic(noSearchResultsTmpl)
}

type searchResultItem struct {
	Index      int
	MemoryType string
	Category   string
	Title      string
	Content    string
	CreatedAt  string
}

func formatSearchResultsList(count int, memories []searchResultItem) string {
	return mustRenderTemplate("search-results-list", searchResultsListTmpl, map[string]any{
		"Count":    count,
		"Memories": memories,
	})
}
