package agent

import (
	_ "embed"
	"strings"

	"github.com/rtc-agent/server/internal/agent/templateutil"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
)

//go:embed prompts/attachments/todo-list.md.tmpl
var todoListTmpl string

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

// userMemoryCategory carries a localized label and its grouped memory items.
type userMemoryCategory struct {
	Label string
	Items []*memory.Memory
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
