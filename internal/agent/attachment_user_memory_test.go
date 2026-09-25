package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserMemoryAttachment_Name(t *testing.T) {
	// P1-2 fix: MemoryAttachment.Name() must return "Memory", not "Session Memory"
	// or "User Memory". The agent should only see a generic "Memory" label.
	a := &UserMemoryAttachment{}
	assert.Equal(t, "Memory", a.Name(),
		"Name() must return 'Memory' (P1-2 fix: no 'Session Memory' or 'User Memory' leak)")
}

func TestUserMemoryPreamble(t *testing.T) {
	tests := []struct {
		name     string
		lang     string
		contains string
	}{
		{
			name:     "Chinese preamble",
			lang:     "zh",
			contains: "持久记忆",
		},
		{
			name:     "English preamble",
			lang:     "en",
			contains: "persistent memories",
		},
		{
			name:     "Unknown language falls back to English",
			lang:     "fr",
			contains: "persistent memories",
		},
		{
			name:     "Empty language falls back to English",
			lang:     "",
			contains: "persistent memories",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := userMemoryPreambleText(tt.lang)
			assert.Contains(t, result, tt.contains)
		})
	}
}

func TestUserMemoryCategoryLabels(t *testing.T) {
	t.Run("Chinese labels", func(t *testing.T) {
		labels := userMemoryLabels("zh")
		assert.Equal(t, "关于用户", labels["user"])
		assert.Equal(t, "工作偏好与反馈", labels["feedback"])
		assert.Equal(t, "项目信息", labels["project"])
		assert.Equal(t, "参考资料", labels["reference"])
	})

	t.Run("English labels", func(t *testing.T) {
		labels := userMemoryLabels("en")
		assert.Equal(t, "About User", labels["user"])
		assert.Equal(t, "Work Preferences & Feedback", labels["feedback"])
		assert.Equal(t, "Project Info", labels["project"])
		assert.Equal(t, "References", labels["reference"])
	})

	t.Run("Unknown language falls back to English", func(t *testing.T) {
		labels := userMemoryLabels("ja")
		assert.Equal(t, "About User", labels["user"])
	})
}

func TestContainsCJKDirective(t *testing.T) {
	assertBoolCases(t, containsCJKDirective, []struct {
		name     string
		text     string
		expected bool
	}{
		{"Direct Chinese directive", "请 always respond in chinese", true},
		{"Chinese directive with 中文", "respond in 中文 please", true},
		{"用中文回复", "用中文回复用户的问题", true},
		{"No directive", "You are a helpful assistant", false},
		{"English only directive", "always respond in english", false},
	})
}

func TestContainsChineseHint(t *testing.T) {
	assertBoolCases(t, containsChineseHint, []struct {
		name     string
		text     string
		expected bool
	}{
		{"Always respond in Chinese", "You should always respond in chinese", true},
		{"Detect user's language", "Detect the user's language and respond accordingly", true},
		{"Heavy CJK content", "这是一个中文助手的配置描述，包含了大量的中文字符来确保能够正确识别语言类型", true},
		{"English only", "You are a helpful coding assistant that writes clean code", false},
		{"Minimal CJK in mostly English", "The word for hello in Chinese is 你好 but everything else is English text that goes on for a while", false},
	})
}

// assertBoolCases runs a table-driven test where each case calls fn(text)
// and asserts the result equals expected.
func assertBoolCases(t *testing.T, fn func(string) bool, cases []struct {
	name     string
	text     string
	expected bool
},
) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, fn(tc.text))
		})
	}
}
