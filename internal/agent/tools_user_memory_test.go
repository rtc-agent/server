package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEstimateMemoryTokens_NonEmpty verifies that estimateMemoryTokens returns
// a positive count for non-empty content. This validates the P2-2 fix where
// saveMemory now sets TokenCount via estimateMemoryTokens(content).
func TestEstimateMemoryTokens_NonEmpty(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantGT  int // result must be > wantGT
	}{
		{"short string", "hello world", 0},
		{"sentence", "The quick brown fox jumps over the lazy dog", 0},
		{"paragraph", strings.Repeat("word ", 100), 0},
		{"chinese", "这是一个中文测试字符串，用于验证令牌计数功能", 0},
		{"mixed", "Hello 你好 World 世界", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateMemoryTokens(tt.content)
			assert.Greater(t, got, tt.wantGT,
				"estimateMemoryTokens(%q) should return > %d, got %d",
				tt.content, tt.wantGT, got)
		})
	}
}

// TestEstimateMemoryTokens_Empty verifies that empty content returns 0 tokens.
func TestEstimateMemoryTokens_Empty(t *testing.T) {
	assert.Equal(t, 0, estimateMemoryTokens(""),
		"empty string should have 0 tokens")
}

func TestValidateStructuredContent(t *testing.T) {
	tests := []struct {
		name           string
		content        string
		wantErr        bool
		wantErrContain string // substring expected in the error message; ignored when wantErr is false
	}{
		// ----------------------------------------------------------------
		// 1. Both sections present -> no error
		// ----------------------------------------------------------------
		{
			name:    "standard format with colons",
			content: "**Why:** some reason\n\n**How to apply:** do something",
		},
		{
			name:    "without colons",
			content: "**Why** some reason\n\n**How to apply** do something",
		},
		{
			name:    "mixed case uppercase",
			content: "**WHY:** some reason\n\n**HOW TO APPLY:** do something",
		},
		{
			name:    "mixed case title case",
			content: "**why:** some reason\n\n**How To Apply:** do something",
		},
		{
			name:    "Why without colon, How to apply with colon",
			content: "**Why** some reason\n\n**How to apply:** do something",
		},
		{
			name:    "Why with colon, How to apply without colon",
			content: "**Why:** some reason\n\n**How to apply** do something",
		},
		{
			name: "both sections with surrounding prose",
			content: `Here is my feedback.

**Why:** The API response times are too slow for real-time usage.

Some extra context here.

**How to apply:** Cache the API responses for 5 minutes and use stale-while-revalidate.

Thanks!`,
		},

		// ----------------------------------------------------------------
		// 2. Missing both sections -> error mentioning both
		// ----------------------------------------------------------------
		{
			name:           "empty content",
			content:        "",
			wantErr:        true,
			wantErrContain: "and",
		},
		{
			name:           "random text without sections",
			content:        "This is just some random text without any required sections.",
			wantErr:        true,
			wantErrContain: "and",
		},
		{
			name:           "only Note section",
			content:        "**Note:** This is a note without the required sections.",
			wantErr:        true,
			wantErrContain: "and",
		},
		{
			name:           "content with bold text but not the required sections",
			content:        "**Important:** something\n\n**Warning:** something else",
			wantErr:        true,
			wantErrContain: "and",
		},

		// ----------------------------------------------------------------
		// 3. Missing only Why -> error mentioning Why
		// ----------------------------------------------------------------
		{
			name:           "has How to apply but missing Why",
			content:        "**How to apply:** do something useful",
			wantErr:        true,
			wantErrContain: "Why",
		},
		{
			name:           "has How to apply without colon but missing Why",
			content:        "**How to apply** do something useful",
			wantErr:        true,
			wantErrContain: "Why",
		},
		{
			name: "has How to apply with extra text but missing Why",
			content: `Some introduction text.

**How to apply:** First do X, then do Y.

Conclusion.`,
			wantErr:        true,
			wantErrContain: "Why",
		},

		// ----------------------------------------------------------------
		// 4. Missing only How to apply -> error mentioning How to apply
		// ----------------------------------------------------------------
		{
			name:           "has Why but missing How to apply",
			content:        "**Why:** because it improves performance",
			wantErr:        true,
			wantErrContain: "How to apply",
		},
		{
			name:           "has Why without colon but missing How to apply",
			content:        "**Why** because it improves performance",
			wantErr:        true,
			wantErrContain: "How to apply",
		},
		{
			name: "has Why with extra text but missing How to apply",
			content: `Background information.

**Why:** The current implementation has a memory leak.

More details about the issue.`,
			wantErr:        true,
			wantErrContain: "How to apply",
		},

		// ----------------------------------------------------------------
		// 5. Edge cases
		// ----------------------------------------------------------------
		{
			name:    "Why embedded in a sentence",
			content: "Let me explain **Why:** this is important and **How to apply:** just do it",
		},
		{
			name:    "extra whitespace around sections",
			content: "  **Why:**   some reason  \n\n  **How to apply:**   do something  ",
		},
		{
			name: "markdown formatting around sections",
			content: `## Feedback

### **Why:**
The feature is confusing.

### **How to apply:**
Simplify the UI.`,
		},
		{
			name:    "sections separated by long content",
			content: "**Why:** reason here\n\n" + strings.Repeat("paragraph of text.\n", 50) + "\n**How to apply:** apply it this way",
		},
		{
			name:    "very long content with sections at the end",
			content: strings.Repeat("x", 5000) + "\n\n**Why:** late reason\n\n**How to apply:** late application",
		},
		{
			name:    "single newline between sections",
			content: "**Why:** reason\n**How to apply:** method",
		},
		{
			name:    "case insensitive Why mixed with case insensitive How",
			content: "**whY:** mixed\n**hOw tO aPpLy:** mixed",
		},
		{
			name:           "partial match Why but not How to apply",
			content:        "**Why:** something\n**How:** something else",
			wantErr:        true,
			wantErrContain: "How to apply",
		},
		{
			name:           "partial match How to apply but not Why",
			content:        "**What:** something\n**How to apply:** something",
			wantErr:        true,
			wantErrContain: "Why",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStructuredContent(tc.content)

			if tc.wantErr {
				assert.Error(t, err, "expected an error but got nil")
				if err != nil && tc.wantErrContain != "" {
					assert.Contains(t, err.Error(), tc.wantErrContain,
						"error message should contain %q", tc.wantErrContain)
				}
			} else {
				assert.NoError(t, err, "expected no error but got: %v", err)
			}
		})
	}
}
