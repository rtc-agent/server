package repo

import (
	"strings"
	"testing"
)

func TestEscapeLikePattern(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain text",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "percent wildcard",
			input:    "100%",
			expected: `100\%`,
		},
		{
			name:     "underscore wildcard",
			input:    "user_name",
			expected: `user\_name`,
		},
		{
			name:     "backslash",
			input:    `path\to\file`,
			expected: `path\\to\\file`,
		},
		{
			name:     "all special chars",
			input:    `100%_of\path`,
			expected: `100\%\_of\\path`,
		},
		{
			name:     "chinese text",
			input:    "中文搜索",
			expected: "中文搜索",
		},
		{
			name:     "mixed wildcards",
			input:    "test%_pattern\\value",
			expected: `test\%\_pattern\\value`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := escapeLikePattern(tt.input)
			if result != tt.expected {
				t.Errorf("escapeLikePattern(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestEscapeLikePatternPreventsWildcardInjection(t *testing.T) {
	// Test 1: Percent wildcard injection
	// Without escaping: query = "%" matches all records
	maliciousQuery := "%"
	escaped := escapeLikePattern(maliciousQuery)
	likeQuery := "%" + escaped + "%"

	// The escaped query should contain the literal \% not just %
	if !strings.Contains(likeQuery, `\%`) {
		t.Errorf("Percent wildcard not escaped: %q", likeQuery)
	}

	// Test 2: Underscore wildcard injection (DoS via pattern matching)
	maliciousQuery = "______________"
	escaped = escapeLikePattern(maliciousQuery)
	likeQuery = "%" + escaped + "%"

	// Should contain escaped underscores
	if !strings.Contains(likeQuery, `\_`) {
		t.Errorf("Underscore wildcard not escaped: %q", likeQuery)
	}

	// Test 3: Combined injection
	maliciousQuery = "%_test_%"
	escaped = escapeLikePattern(maliciousQuery)
	likeQuery = "%" + escaped + "%"

	// Should escape both % and _
	if !strings.Contains(likeQuery, `\%`) || !strings.Contains(likeQuery, `\_`) {
		t.Errorf("Combined wildcards not properly escaped: %q", likeQuery)
	}
}

func TestEscapeLikePatternPreservesNormalQueries(t *testing.T) {
	// Normal search queries should not be affected
	normalQueries := []string{
		"hello",
		"world",
		"test123",
		"search term",
		"中文搜索",
		"mixed 混合 query",
	}

	for _, query := range normalQueries {
		escaped := escapeLikePattern(query)
		if escaped != query {
			t.Errorf("Normal query modified: %q -> %q", query, escaped)
		}
	}
}
