package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
			result := userMemoryPreamble(tt.lang)
			assert.Contains(t, result, tt.contains)
		})
	}
}

func TestUserMemoryCategoryLabels(t *testing.T) {
	t.Run("Chinese labels", func(t *testing.T) {
		labels := userMemoryCategoryLabels("zh")
		assert.Equal(t, "关于用户", labels["user"])
		assert.Equal(t, "工作偏好与反馈", labels["feedback"])
		assert.Equal(t, "项目信息", labels["project"])
		assert.Equal(t, "参考资料", labels["reference"])
	})

	t.Run("English labels", func(t *testing.T) {
		labels := userMemoryCategoryLabels("en")
		assert.Equal(t, "About User", labels["user"])
		assert.Equal(t, "Work Preferences & Feedback", labels["feedback"])
		assert.Equal(t, "Project Info", labels["project"])
		assert.Equal(t, "References", labels["reference"])
	})

	t.Run("Unknown language falls back to English", func(t *testing.T) {
		labels := userMemoryCategoryLabels("ja")
		assert.Equal(t, "About User", labels["user"])
	})
}

func TestContainsCJKDirective(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{
			name:     "Direct Chinese directive",
			text:     "请 always respond in chinese",
			expected: true,
		},
		{
			name:     "Chinese directive with 中文",
			text:     "respond in 中文 please",
			expected: true,
		},
		{
			name:     "用中文回复",
			text:     "用中文回复用户的问题",
			expected: true,
		},
		{
			name:     "No directive",
			text:     "You are a helpful assistant",
			expected: false,
		},
		{
			name:     "English only directive",
			text:     "always respond in english",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsCJKDirective(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestContainsChineseHint(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{
			name:     "Always respond in Chinese",
			text:     "You should always respond in chinese",
			expected: true,
		},
		{
			name:     "Detect user's language",
			text:     "Detect the user's language and respond accordingly",
			expected: true,
		},
		{
			name:     "Heavy CJK content",
			text:     "这是一个中文助手的配置描述，包含了大量的中文字符来确保能够正确识别语言类型",
			expected: true,
		},
		{
			name:     "English only",
			text:     "You are a helpful coding assistant that writes clean code",
			expected: false,
		},
		{
			name:     "Minimal CJK in mostly English",
			text:     "The word for hello in Chinese is 你好 but everything else is English text that goes on for a while",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsChineseHint(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}
