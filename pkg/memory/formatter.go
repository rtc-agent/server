package memory

import (
	"fmt"
	"strings"
)

// Formatter 将 Memory 格式化为不同用途的文本
type Formatter struct{}

// NewFormatter 创建格式化器
func NewFormatter() *Formatter {
	return &Formatter{}
}

// typeOrder defines the display order for memory types in injection.
var typeOrder = []string{
	"context", "progress", "decision", "issue", "learnings",
	"user", "feedback", "project", "reference",
}

// FormatForInjection 格式化为 LLM 注入格式
//
// 参考 formatSessionMemoriesForInjection，但适配统一 Memory 模型。
// 使用 <system-reminder> 标签包裹，每类别最多 3 条，内容截断 200 字符。
// language 参数预留用于未来多语言支持（当前未使用）。
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
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			fmt.Fprintf(&sb, "- **[%s]** %s: %s\n", memType, item.Title, content)
		}
	}

	sb.WriteString("</system-reminder>")
	return sb.String()
}

// FormatForSummary 格式化为压缩摘要格式
//
// 参考 buildSummaryFromMemories，用于 context 压缩的零成本路径。
// 按类型分组输出完整内容（不截断），markdown 格式。
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
	for _, item := range grouped[""] {
		// This won't trigger; empty-key items shouldn't exist after validation.
		_ = item
	}
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

// FormatForExport 格式化为 OKF frontmatter + markdown
//
// 参考设计文档中的导出能力设计章节。
// 生成符合 OKF v0.2 规范的 YAML frontmatter + markdown body。
func (f *Formatter) FormatForExport(m *Memory) string {
	var sb strings.Builder

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

	// Trust: generated info
	sb.WriteString("generated:\n")
	fmt.Fprintf(&sb, "  by: rtc-agent/1.0\n")
	fmt.Fprintf(&sb, "  at: \"%s\"\n", m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))

	sb.WriteString("---\n\n")

	// Markdown body
	if m.Title != "" {
		fmt.Fprintf(&sb, "# %s\n\n", m.Title)
	}
	sb.WriteString(m.Content)
	sb.WriteString("\n")

	return sb.String()
}

// yamlQuote 对 YAML 标量值进行安全引用。
// 当值包含 YAML 特殊字符（冒号、引号、#等）时，用双引号包裹并转义内部引号；
// 否则原样返回，保持 frontmatter 的可读性。
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
	// 双引号包裹，转义内部双引号和反斜杠
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
