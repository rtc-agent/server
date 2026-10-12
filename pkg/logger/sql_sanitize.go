package logger

import (
	"regexp"
)

// Regex patterns for SQL sanitization (compiled once and reused)
var (
	// Pattern order matters: UUID must come before generic numbers
	// (?i) makes UUID pattern case-insensitive to catch both lowercase and uppercase UUIDs
	uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// Support both backslash escape (\') and PostgreSQL standard escape ('')
	// Order matters: try backslash escape first, then double single-quote
	stringPattern = regexp.MustCompile(`'(?:[^']|\\.|'')*'`)
	numberPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
)

// sanitizeSQL replaces literal values in SQL statements with placeholders to prevent
// sensitive data (passwords, tokens, keys) from being leaked to tracing backends.
// Returns a sanitized SQL string with:
// - Single-quoted strings replaced with '$?'
// - UUID values replaced with '$?'
// - Numeric literals replaced with $?
func sanitizeSQL(sql string) string {
	if sql == "" {
		return sql
	}

	// Step 1: Replace single-quoted strings (handles escaped quotes)
	result := stringPattern.ReplaceAllString(sql, "'$?'")

	// Step 2: Replace UUIDs (must come before generic number replacement)
	result = uuidPattern.ReplaceAllString(result, "'$?'")

	// Step 3: Replace numeric literals (integers and decimals)
	result = numberPattern.ReplaceAllString(result, "$?")

	return result
}
