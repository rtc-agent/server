package logger

import (
	"testing"
)

func TestSanitizeSQL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "SELECT with string literal",
			input:    "SELECT * FROM users WHERE name = 'John Doe'",
			expected: "SELECT * FROM users WHERE name = '$?'",
		},
		{
			name:     "SELECT with multiple string literals",
			input:    "SELECT * FROM users WHERE name = 'Alice' AND email = 'alice@example.com'",
			expected: "SELECT * FROM users WHERE name = '$?' AND email = '$?'",
		},
		{
			name:     "SELECT with escaped quotes",
			input:    "SELECT * FROM users WHERE name = 'O\\'Brien'",
			expected: "SELECT * FROM users WHERE name = '$?'",
		},
		{
			name:     "SELECT with numeric literal",
			input:    "SELECT * FROM users WHERE age = 25",
			expected: "SELECT * FROM users WHERE age = $?",
		},
		{
			name:     "SELECT with decimal number",
			input:    "SELECT * FROM products WHERE price = 99.99",
			expected: "SELECT * FROM products WHERE price = $?",
		},
		{
			name:     "SELECT with UUID",
			input:    "SELECT * FROM users WHERE id = '550e8400-e29b-41d4-a716-446655440000'",
			expected: "SELECT * FROM users WHERE id = '$?'",
		},
		{
			name:     "INSERT with string values",
			input:    "INSERT INTO users (name, email) VALUES ('Bob', 'bob@example.com')",
			expected: "INSERT INTO users (name, email) VALUES ('$?', '$?')",
		},
		{
			name:     "INSERT with mixed types",
			input:    "INSERT INTO users (name, age, balance) VALUES ('Charlie', 30, 1000.50)",
			expected: "INSERT INTO users (name, age, balance) VALUES ('$?', $?, $?)",
		},
		{
			name:     "INSERT with UUID",
			input:    "INSERT INTO sessions (id, user_id) VALUES ('550e8400-e29b-41d4-a716-446655440001', '550e8400-e29b-41d4-a716-446655440002')",
			expected: "INSERT INTO sessions (id, user_id) VALUES ('$?', '$?')",
		},
		{
			name:     "UPDATE with string literal",
			input:    "UPDATE users SET name = 'David' WHERE id = 123",
			expected: "UPDATE users SET name = '$?' WHERE id = $?",
		},
		{
			name:     "UPDATE with multiple fields",
			input:    "UPDATE users SET name = 'Eve', email = 'eve@example.com', age = 28 WHERE id = 456",
			expected: "UPDATE users SET name = '$?', email = '$?', age = $? WHERE id = $?",
		},
		{
			name:     "DELETE with string condition",
			input:    "DELETE FROM users WHERE email = 'frank@example.com'",
			expected: "DELETE FROM users WHERE email = '$?'",
		},
		{
			name:     "DELETE with numeric condition",
			input:    "DELETE FROM users WHERE id = 789",
			expected: "DELETE FROM users WHERE id = $?",
		},
		{
			name:     "DELETE with UUID condition",
			input:    "DELETE FROM sessions WHERE user_id = '550e8400-e29b-41d4-a716-446655440003'",
			expected: "DELETE FROM sessions WHERE user_id = '$?'",
		},
		{
			name:     "complex query with IN clause",
			input:    "SELECT * FROM users WHERE id IN (1, 2, 3, 4, 5)",
			expected: "SELECT * FROM users WHERE id IN ($?, $?, $?, $?, $?)",
		},
		{
			name:     "query with LIKE",
			input:    "SELECT * FROM users WHERE name LIKE '%john%'",
			expected: "SELECT * FROM users WHERE name LIKE '$?'",
		},
		{
			name:     "query with password field",
			input:    "INSERT INTO users (username, password) VALUES ('admin', 'SuperSecret123!')",
			expected: "INSERT INTO users (username, password) VALUES ('$?', '$?')",
		},
		{
			name:     "query with token field",
			input:    "UPDATE sessions SET token = 'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9' WHERE user_id = 100",
			expected: "UPDATE sessions SET token = '$?' WHERE user_id = $?",
		},
		{
			name:     "query with API key",
			input:    "SELECT * FROM api_keys WHERE key = 'sk_live_abc123def456'",
			expected: "SELECT * FROM api_keys WHERE key = '$?'",
		},
		{
			name:     "query without literals",
			input:    "SELECT * FROM users",
			expected: "SELECT * FROM users",
		},
		{
			name:     "query with column numbers in names",
			input:    "SELECT field1, field2 FROM table1 WHERE id = 1",
			expected: "SELECT field1, field2 FROM table1 WHERE id = $?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeSQL(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeSQL() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func BenchmarkSanitizeSQL(b *testing.B) {
	testSQL := "INSERT INTO users (name, email, age, balance) VALUES ('John Doe', 'john@example.com', 30, 1000.50)"

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sanitizeSQL(testSQL)
	}
}
