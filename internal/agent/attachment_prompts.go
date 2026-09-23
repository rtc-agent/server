// attachment_prompts.go — Context attachment templates.
//
// Attachments are content blocks injected into the LLM context at the start of
// each turn (e.g. session memories, user memories). These templates
// format that content as Markdown or XML for consumption by the model.
//
// Note: formatTodoList is still defined here and used by todoWriteTool to
// produce the tool_result content, even though TodoList is no longer an
// attachment (it is persisted as tool_result messages via publishToolMessages).
//
// Naming convention:
//   - Variables: <name>Tmpl (e.g. todoListTmpl, sessionMemoryInjectionTmpl)
//   - Accessors: format<Name>(args...) string (e.g. formatTodoList, formatSessionMemoryInjection)
//   - Files: prompts/attachments/<name>.md.tmpl (all use text/template syntax)
package agent

import (
	_ "embed"
	"sort"
	"strings"

	"github.com/rtc-agent/server/internal/agent/stringutil"
	"github.com/rtc-agent/server/internal/agent/templateutil"
	"github.com/rtc-agent/server/internal/model"
)

//go:embed prompts/attachments/todo-list.md.tmpl
var todoListTmpl string

//go:embed prompts/attachments/session-memory-injection.md.tmpl
var sessionMemoryInjectionTmpl string

//go:embed prompts/attachments/session-memory-summary.md.tmpl
var sessionMemorySummaryTmpl string

//go:embed prompts/attachments/user-memory-wrapper.md.tmpl
var userMemoryWrapperTmpl string

//go:embed prompts/attachments/user-memory-preamble-en.md
var userMemoryPreambleEn string

//go:embed prompts/attachments/user-memory-preamble-zh.md
var userMemoryPreambleZh string

// formatTodoList renders the todo list as a Markdown section grouped by status.
//
// Tasks are grouped into In Progress (bold), Pending (plain), and Completed
// (strikethrough). Empty groups are omitted from the output.
//
// Used by todoWriteTool to produce the tool_result content.
// Previously also used by TodoListAttachment (removed).
func formatTodoList(todos []model.TodoItem) string {
	var inProgress, pending, completed []model.TodoItem
	for _, t := range todos {
		switch t.Status {
		case "in_progress":
			inProgress = append(inProgress, t)
		case "pending":
			pending = append(pending, t)
		case "completed":
			completed = append(completed, t)
		}
	}
	return templateutil.MustRender("todo-list", todoListTmpl, map[string]any{
		"InProgress": inProgress,
		"Pending":    pending,
		"Completed":  completed,
	})
}

// injectionItem is a flat view of a single memory for template rendering.
type injectionItem struct {
	Category string
	Title    string
	Content  string
}

// summaryCategory carries a display name and its grouped memory items.
type summaryCategory struct {
	DisplayName string
	Items       []*model.SessionMemory
}

// userMemoryCategory carries a localized label and its grouped memory items.
type userMemoryCategory struct {
	Label string
	Items []*model.UserMemory
}

// formatSessionMemoryInjection renders session memories as a system-reminder
// block for injection at the start of each turn.
//
// Each category shows at most 3 items, and content is truncated to 200 chars.
func formatSessionMemoryInjection(memories []*model.SessionMemory) string {
	if len(memories) == 0 {
		return ""
	}

	grouped := make(map[string][]*model.SessionMemory)
	for _, mem := range memories {
		grouped[mem.Category] = append(grouped[mem.Category], mem)
	}

	// Collect items per category, capped at 3 and truncated.
	var items []injectionItem
	categories := []string{
		model.SessionMemoryCategoryContext,
		model.SessionMemoryCategoryProgress,
		model.SessionMemoryCategoryDecision,
		model.SessionMemoryCategoryIssue,
		model.SessionMemoryCategoryLearnings,
	}
	for _, cat := range categories {
		mems, ok := grouped[cat]
		if !ok || len(mems) == 0 {
			continue
		}
		limit := 3
		if len(mems) < limit {
			limit = len(mems)
		}
		for i := 0; i < limit; i++ {
			items = append(items, injectionItem{
				Category: cat,
				Title:    mems[i].Title,
				Content:  stringutil.TruncateByByte(mems[i].Content, 200),
			})
		}
	}

	if len(items) == 0 {
		return ""
	}

	return templateutil.MustRender("session-memory-injection", sessionMemoryInjectionTmpl, map[string]any{
		"Items": items,
	})
}

// buildSummaryFromMemoriesTmpl renders session memories as a Markdown summary,
// grouped by category with newest items first within each group.
func buildSummaryFromMemoriesTmpl(memories []*model.SessionMemory) string {
	if len(memories) == 0 {
		return ""
	}

	grouped := make(map[string][]*model.SessionMemory)
	for _, mem := range memories {
		grouped[mem.Category] = append(grouped[mem.Category], mem)
	}

	// Sort each group by created_at descending (newest first).
	for _, items := range grouped {
		sortByCreatedAtDesc(items)
	}

	// Build ordered category list with display names.
	categories := []struct {
		key         string
		displayName string
	}{
		{model.SessionMemoryCategoryContext, "Current Context"},
		{model.SessionMemoryCategoryProgress, "Progress"},
		{model.SessionMemoryCategoryDecision, "Decisions"},
		{model.SessionMemoryCategoryIssue, "Issues & Solutions"},
		{model.SessionMemoryCategoryLearnings, "Learnings"},
	}

	var cats []summaryCategory
	for _, cat := range categories {
		items, ok := grouped[cat.key]
		if !ok || len(items) == 0 {
			continue
		}
		cats = append(cats, summaryCategory{
			DisplayName: cat.displayName,
			Items:       items,
		})
	}

	if len(cats) == 0 {
		return ""
	}

	return templateutil.MustRender("session-memory-summary", sessionMemorySummaryTmpl, map[string]any{
		"Categories": cats,
	})
}

// formatUserMemoryWrapper renders the user memory XML wrapper with preamble
// and category-grouped memory items.
//
// The inner content (preamble + category blocks) is pre-formatted in Go so that
// the template stays trivial and the exact newline layout is easy to verify.
func formatUserMemoryWrapper(lang string, categories []userMemoryCategory) string {
	preamble := userMemoryPreambleText(lang)
	content := buildUserMemoryContent(preamble, categories)
	return templateutil.MustRender("user-memory-wrapper", userMemoryWrapperTmpl, map[string]any{
		"Content": content,
	})
}

// buildUserMemoryContent joins the preamble and category blocks into a single
// string with one blank line separating each section.
//
// Layout per category block (mirrors the original formatUserMemoryForInjection):
//
//	## <label>\n
//	### <title>\n
//	<content>\n
//	\n
//
// One blank line separates the preamble from the first category, adjacent
// categories, and the last category from the closing </user_memory> tag.
func buildUserMemoryContent(preamble string, categories []userMemoryCategory) string {
	var sb strings.Builder
	sb.WriteString(preamble)
	sb.WriteString("\n")
	for _, cat := range categories {
		sb.WriteString("\n## ")
		sb.WriteString(cat.Label)
		sb.WriteString("\n")
		for _, mem := range cat.Items {
			sb.WriteString("### ")
			sb.WriteString(mem.Title)
			sb.WriteString("\n")
			sb.WriteString(mem.Content)
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// userMemoryPreambleText returns the localized preamble for user memory.
func userMemoryPreambleText(lang string) string {
	switch lang {
	case "zh":
		return userMemoryPreambleZh
	default:
		return userMemoryPreambleEn
	}
}

// userMemoryLabels returns localized category labels for user memory.
func userMemoryLabels(lang string) map[string]string {
	// The embedded template is used only as a reference; the actual labels are
	// returned as a map so callers can index by category key.
	switch lang {
	case "zh":
		return map[string]string{
			model.UserMemoryCategoryUser:      "关于用户",
			model.UserMemoryCategoryFeedback:  "工作偏好与反馈",
			model.UserMemoryCategoryProject:   "项目信息",
			model.UserMemoryCategoryReference: "参考资料",
		}
	default:
		return map[string]string{
			model.UserMemoryCategoryUser:      "About User",
			model.UserMemoryCategoryFeedback:  "Work Preferences & Feedback",
			model.UserMemoryCategoryProject:   "Project Info",
			model.UserMemoryCategoryReference: "References",
		}
	}
}

// sortByCreatedAtDesc sorts session memories by created_at in descending order
// (newest first).
func sortByCreatedAtDesc(items []*model.SessionMemory) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
}
