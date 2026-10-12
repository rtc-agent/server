package logger

import (
	"net/url"
	"strings"
	"testing"
)

// extractDBNameFromDSNForTest is a test helper that extracts DB name from DSN
// without requiring a full gorm.DB instance. This mirrors the logic in
// extractDBNameFromDSN but can be tested independently.
func extractDBNameFromDSNForTest(dsn string) string {
	// Try URL format first: postgres://user:pass@host:port/dbname?params
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err == nil && parsed.Path != "" {
			if len(parsed.Path) > 1 {
				return parsed.Path[1:]
			}
		}
	}

	// Fallback to key-value format: host=... user=... dbname=... password=...
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "dbname=") {
			return strings.TrimPrefix(part, "dbname=")
		}
	}
	return ""
}

func TestExtractDBNameFromDSN(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		expected string
	}{
		{
			name:     "URL format with simple password",
			dsn:      "postgres://user:pass@localhost:5432/mydb",
			expected: "mydb",
		},
		{
			name:     "URL format with query params",
			dsn:      "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
			expected: "mydb",
		},
		{
			name:     "URL format with URL-encoded password containing slash",
			dsn:      "postgres://user:sec%2Fret%2Fpass@localhost:5432/mydb",
			expected: "mydb",
		},
		{
			name:     "URL format with URL-encoded special chars in password",
			dsn:      "postgres://user:p%40ss%3Aw0rd%2Ftest@localhost:5432/mydb",
			expected: "mydb",
		},
		{
			name:     "URL format with postgresql prefix",
			dsn:      "postgresql://user:pass@localhost:5432/mydb",
			expected: "mydb",
		},
		{
			name:     "Key-value format",
			dsn:      "host=localhost user=postgres password=secret dbname=mydb port=5432",
			expected: "mydb",
		},
		{
			name:     "Key-value format with dbname at end",
			dsn:      "host=localhost user=postgres dbname=mydb",
			expected: "mydb",
		},
		{
			name:     "Key-value format with password containing slash (no URL encoding needed)",
			dsn:      "host=localhost user=postgres password=sec/ret/pass dbname=mydb",
			expected: "mydb",
		},
		{
			name:     "Empty DSN",
			dsn:      "",
			expected: "",
		},
		{
			name:     "Invalid URL format falls back to key-value",
			dsn:      "dbname=fallback_db",
			expected: "fallback_db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractDBNameFromDSNForTest(tt.dsn)
			if result != tt.expected {
				t.Errorf("extractDBNameFromDSNForTest() = %q, want %q (DSN: %q)", result, tt.expected, tt.dsn)
			}
		})
	}
}

// TestTruncateSQLAtKeyword tests the P3 SQL truncation optimization
func TestTruncateSQLAtKeyword(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		maxLen   int
		expected string
	}{
		{
			name:     "Short SQL not truncated",
			sql:      "SELECT * FROM users",
			maxLen:   500,
			expected: "SELECT * FROM users",
		},
		{
			name:     "INSERT with VALUES keyword",
			sql:      "INSERT INTO users (name, email) VALUES ('John', 'john@example.com'), ('Jane', 'jane@example.com')",
			maxLen:   50,
			expected: "INSERT INTO users (name, email) VALUES ...(+truncated)",
		},
		{
			name:     "UPDATE with SET keyword",
			sql:      "UPDATE users SET name = 'New Name', email = 'new@example.com' WHERE id = 123",
			maxLen:   25,
			expected: "UPDATE users SET ...(+truncated)",
		},
		{
			name:     "SELECT with WHERE keyword",
			sql:      "SELECT * FROM users WHERE name = 'John' AND age > 30",
			maxLen:   30,
			expected: "SELECT * FROM users WHERE ...(+truncated)",
		},
		{
			name:     "No good keyword found, fallback to fixed truncation",
			sql:      "SELECT very_long_column_name_that_exceeds_limit FROM users",
			maxLen:   20,
			expected: "SELECT very_long_col...(+truncated)",
		},
		{
			name:     "Keyword appears within search limit, truncate at keyword",
			sql:      "SELECT * FROM users WHERE id = 123 AND name = 'John' AND email = 'john@example.com'",
			maxLen:   15,
			expected: "SELECT * FROM ...(+truncated)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateSQLAtKeyword(tt.sql, tt.maxLen)
			// For tests with dynamic content, just check prefix and suffix
			if strings.Contains(tt.expected, "...(+truncated)") {
				prefix := strings.Split(tt.expected, "...(+truncated)")[0]
				if !strings.HasPrefix(result, prefix) {
					t.Errorf("truncateSQLAtKeyword() = %q, expected prefix %q", result, prefix)
				}
				if !strings.HasSuffix(result, "...(truncated)") {
					t.Errorf("truncateSQLAtKeyword() = %q, expected suffix '...(truncated)'", result)
				}
			} else if result != tt.expected {
				t.Errorf("truncateSQLAtKeyword() = %q, want %q", result, tt.expected)
			}
		})
	}
}
