package memory

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Formatter formats Memory objects into text for various purposes.
type Formatter struct{}

// NewFormatter creates a new Formatter.
func NewFormatter() *Formatter {
	return &Formatter{}
}

// typeOrder defines the display order for memory types in injection.
var typeOrder = []string{
	"context", "progress", "decision", "issue", "learnings",
	"user", "feedback", "project", "reference",
}

// FormatForInjection formats memories for LLM context injection.
//
// Modeled after formatSessionMemoriesForInjection but adapted for the
// unified Memory model. Wraps output in <system-reminder> tags, limits
// each type to at most 3 entries, and truncates content at 200 runes.
// The language parameter is reserved for future multilingual support
// (currently unused; always emits English tags).
// NOTE: The language parameter is reserved but unused. If multilingual
// support is needed in the future, tag text can be switched based on
// the language value (e.g., Chinese labels). All current LLMs understand
// English tags, so switching is not implemented yet.
func (f *Formatter) FormatForInjection(memories []*Memory, language string) string {
	if len(memories) == 0 {
		return ""
	}

	// Group by type
	grouped := make(map[string][]*Memory)
	for _, mem := range memories {
		grouped[mem.Type] = append(grouped[mem.Type], mem)
	}

	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("## Session Memory (Current Session)\n\n")
	sb.WriteString("Key information from this session:\n\n")

	for _, memType := range typeOrder {
		items, ok := grouped[memType]
		if !ok || len(items) == 0 {
			continue
		}

		// Each type: max 3 items
		limit := 3
		if len(items) < limit {
			limit = len(items)
		}

		for i := 0; i < limit; i++ {
			item := items[i]
			content := item.Content
			runes := []rune(content)
			if len(runes) > 200 {
				content = string(runes[:200]) + "..."
			}
			fmt.Fprintf(&sb, "- **[%s]** %s: %s\n", memType, item.Title, content)
		}
	}

	sb.WriteString("</system-reminder>")
	return sb.String()
}

// FormatForSummary formats memories for context compression summary.
//
// Modeled after buildSummaryFromMemories; used as the zero-cost path
// for context compression. Outputs full content (no truncation) grouped
// by type in markdown format.
func (f *Formatter) FormatForSummary(memories []*Memory) string {
	if len(memories) == 0 {
		return ""
	}

	// Group by type
	grouped := make(map[string][]*Memory)
	for _, mem := range memories {
		grouped[mem.Type] = append(grouped[mem.Type], mem)
	}

	var sb strings.Builder
	sb.WriteString("# Session Memory\n\n")
	sb.WriteString("This is a summary of the current session, extracted from the conversation history.\n\n")

	// Type order and display names — covers all ValidMemoryTypes.
	// Unknown types (future extensions) are collected under "Other".
	types := []struct {
		key         string
		displayName string
	}{
		{"context", "Current Context"},
		{"progress", "Progress"},
		{"decision", "Decisions"},
		{"issue", "Issues & Solutions"},
		{"learnings", "Learnings"},
		{"user", "About User"},
		{"feedback", "Feedback & Preferences"},
		{"project", "Project Info"},
		{"reference", "References"},
	}

	// Collect unknown types not covered by the explicit list above.
	knownTypes := make(map[string]bool, len(types))
	for _, t := range types {
		knownTypes[t.key] = true
	}
	var unknownItems []*Memory
	for memType, items := range grouped {
		if !knownTypes[memType] {
			unknownItems = append(unknownItems, items...)
		}
	}

	for _, t := range types {
		items, ok := grouped[t.key]
		if !ok || len(items) == 0 {
			continue
		}

		fmt.Fprintf(&sb, "## %s\n\n", t.displayName)
		for _, item := range items {
			fmt.Fprintf(&sb, "### %s\n\n", item.Title)
			sb.WriteString(item.Content)
			sb.WriteString("\n\n")
		}
	}

	// Render unknown types under a catch-all heading.
	if len(unknownItems) > 0 {
		sb.WriteString("## Other\n\n")
		for _, item := range unknownItems {
			fmt.Fprintf(&sb, "### %s\n\n", item.Title)
			sb.WriteString(item.Content)
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// writeProvenanceBlock renders the OKF sources block.
func writeProvenanceBlock(sb *strings.Builder, metadata map[string]any) {
	sources, ok := metadata["sources"].([]any)
	if !ok || len(sources) == 0 {
		return
	}
	sb.WriteString("sources:\n")
	for _, src := range sources {
		if s, ok := src.(map[string]any); ok {
			sb.WriteString("  - \n")
			for k, v := range s {
				fmt.Fprintf(sb, "    %s: %v\n", k, v)
			}
		}
	}
}

// writeTrustBlock renders the OKF generated/verified blocks.
func writeTrustBlock(sb *strings.Builder, metadata map[string]any, createdAt time.Time) {
	sb.WriteString("generated:\n")
	generatedBy := "rtc-agent/1.0"
	if gen, ok := metadata["generated"].(map[string]any); ok {
		if by, ok := gen["by"].(string); ok {
			generatedBy = by
		}
	}
	fmt.Fprintf(sb, "  by: %s\n", generatedBy)
	fmt.Fprintf(sb, "  at: \"%s\"\n", createdAt.UTC().Format(time.RFC3339))

	verified, ok := metadata["verified"].([]any)
	if !ok || len(verified) == 0 {
		return
	}
	sb.WriteString("verified:\n")
	for _, v := range verified {
		if vMap, ok := v.(map[string]any); ok {
			sb.WriteString("  - \n")
			for k, val := range vMap {
				fmt.Fprintf(sb, "    %s: %v\n", k, val)
			}
		}
	}
}

// writeLifecycleBlock renders the OKF status/stale_after block.
func writeLifecycleBlock(sb *strings.Builder, metadata map[string]any) {
	if status, ok := metadata["status"].(string); ok {
		fmt.Fprintf(sb, "status: %s\n", status)
	} else {
		sb.WriteString("status: stable\n")
	}
	if staleAfter, ok := metadata["stale_after"].(string); ok {
		fmt.Fprintf(sb, "stale_after: %s\n", staleAfter)
	}
}

// FormatForExport formats a Memory as OKF frontmatter + markdown body.
//
// Produces YAML frontmatter + markdown body conforming to the OKF v0.2
// specification. Extracts Provenance (sources), Trust (generated/verified),
// and Lifecycle (status/stale_after) fields from Metadata and renders
// them into the frontmatter.
func (f *Formatter) FormatForExport(m *Memory) string {
	var sb strings.Builder

	// Parse Metadata
	var metadata map[string]any
	if m.Metadata != "" {
		_ = json.Unmarshal([]byte(m.Metadata), &metadata)
	}

	// YAML frontmatter
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "type: %s\n", m.Type)
	if m.Title != "" {
		fmt.Fprintf(&sb, "title: %s\n", yamlQuote(m.Title))
	}
	if m.Description != "" {
		fmt.Fprintf(&sb, "description: %s\n", yamlQuote(m.Description))
	}
	if len(m.Tags) > 0 {
		sb.WriteString("tags:\n")
		for _, tag := range m.Tags {
			fmt.Fprintf(&sb, "  - %s\n", tag)
		}
	}
	if m.Resource != "" {
		fmt.Fprintf(&sb, "resource: %s\n", m.Resource)
	}

	if metadata != nil {
		writeProvenanceBlock(&sb, metadata)
		writeTrustBlock(&sb, metadata, m.CreatedAt)
		writeLifecycleBlock(&sb, metadata)
	} else {
		sb.WriteString("generated:\n")
		fmt.Fprintf(&sb, "  by: rtc-agent/1.0\n")
		fmt.Fprintf(&sb, "  at: \"%s\"\n", m.CreatedAt.UTC().Format(time.RFC3339))
		sb.WriteString("status: stable\n")
	}

	sb.WriteString("---\n\n")

	// Markdown body
	if m.Title != "" {
		fmt.Fprintf(&sb, "# %s\n\n", m.Title)
	}
	sb.WriteString(m.Content)
	sb.WriteString("\n")

	return sb.String()
}

// yamlQuote safely quotes a YAML scalar value.
// When the value contains YAML special characters (colons, quotes, #,
// etc.), it wraps the value in double quotes and escapes internal quotes;
// otherwise it returns the value as-is to keep the frontmatter readable.
func yamlQuote(s string) string {
	needsQuote := false
	for _, c := range s {
		switch c {
		case ':', '#', '"', '\'', '{', '}', '[', ']', ',', '&', '*', '?', '|', '-', '<', '>', '=', '!', '%', '@', '`':
			needsQuote = true
		}
	}
	if !needsQuote {
		return s
	}
	// Wrap in double quotes, escaping internal double quotes and backslashes.
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
