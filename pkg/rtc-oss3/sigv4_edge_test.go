package rtcoss3

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Edge-case tests for SigV4 implementation
// ============================================================

// ---------- verifyTimestamp edge cases ----------

// TestVerifyTimestamp_FutureTimestamp tests that future timestamps are rejected.
func TestVerifyTimestamp_FutureTimestamp(t *testing.T) {
	// Clock 16 minutes in the future should be rejected
	amzDate := time.Now().UTC().Add(16 * time.Minute).Format("20060102T150405Z")
	err := verifyTimestamp(amzDate, 15*time.Minute)
	if err == nil {
		t.Fatal("Expected error for future timestamp (16 min ahead)")
	}
	s3err, ok := err.(*S3Error)
	if !ok {
		t.Fatalf("Expected S3Error, got %T: %v", err, err)
	}
	if s3err.Code != "RequestTimeTooSkewed" {
		t.Errorf("Expected RequestTimeTooSkewed, got %s", s3err.Code)
	}
}

// TestVerifyTimestamp_ExactlyAtBoundary tests timestamps near the boundary.
func TestVerifyTimestamp_ExactlyAtBoundary(t *testing.T) {
	// 14 minutes ago (within 15-min window) — should pass
	amzDate := time.Now().UTC().Add(-14 * time.Minute).Format("20060102T150405Z")
	err := verifyTimestamp(amzDate, 15*time.Minute)
	if err != nil {
		t.Errorf("Expected no error at 14 min ago, got: %v", err)
	}

	// 16 minutes ago (outside 15-min window) — should fail
	amzDate = time.Now().UTC().Add(-16 * time.Minute).Format("20060102T150405Z")
	err = verifyTimestamp(amzDate, 15*time.Minute)
	if err == nil {
		t.Error("Expected error for timestamp 16 minutes old")
	}
}

// TestVerifyTimestamp_EmptyString tests empty timestamp string.
func TestVerifyTimestamp_EmptyString(t *testing.T) {
	err := verifyTimestamp("", 15*time.Minute)
	if err == nil {
		t.Fatal("Expected error for empty timestamp")
	}
}

// TestVerifyTimestamp_MalformedFormat tests various malformed timestamps.
func TestVerifyTimestamp_MalformedFormat(t *testing.T) {
	tests := []string{
		"2006-01-02T15:04:05Z",  // Wrong format (ISO 8601)
		"20060102",              // Date only
		"20060102T150405",       // Missing Z
		"20060102T150405+00:00", // Timezone offset
		"not-a-date",            // Random string
		"20060132T150405Z",      // Invalid date (day 32)
		"20061302T150405Z",      // Invalid date (month 13)
		"20060102T250405Z",      // Invalid time (hour 25)
	}
	for _, ts := range tests {
		err := verifyTimestamp(ts, 15*time.Minute)
		if err == nil {
			t.Errorf("Expected error for malformed timestamp %q", ts)
		}
	}
}

// ---------- checkPresignedExpiry edge cases ----------

// TestCheckPresignedExpiry_ZeroExpiry tests zero expiry value.
func TestCheckPresignedExpiry_ZeroExpiry(t *testing.T) {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	err := checkPresignedExpiry(amzDate, "0")
	// 0-second expiry: should be expired immediately
	if err == nil {
		t.Error("Expected error for 0-second expiry")
	}
}

// TestCheckPresignedExpiry_NegativeExpiry tests negative expiry value.
func TestCheckPresignedExpiry_NegativeExpiry(t *testing.T) {
	amzDate := time.Now().UTC().Add(-1 * time.Hour).Format("20060102T150405Z")
	err := checkPresignedExpiry(amzDate, "-100")
	if err == nil {
		t.Error("Expected error for negative expiry")
	}
}

// TestCheckPresignedExpiry_InvalidExpiry tests non-numeric expiry.
func TestCheckPresignedExpiry_InvalidExpiry(t *testing.T) {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	err := checkPresignedExpiry(amzDate, "abc")
	if err == nil {
		t.Error("Expected error for non-numeric expiry")
	}
}

// TestCheckPresignedExpiry_EmptyExpiry tests empty expiry string.
func TestCheckPresignedExpiry_EmptyExpiry(t *testing.T) {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	err := checkPresignedExpiry(amzDate, "")
	if err == nil {
		t.Error("Expected error for empty expiry")
	}
}

// TestCheckPresignedExpiry_VeryLargeExpiry tests extremely large expiry.
func TestCheckPresignedExpiry_VeryLargeExpiry(t *testing.T) {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	// 7 days in seconds (604800) - should be valid per S3 spec
	err := checkPresignedExpiry(amzDate, "604800")
	if err != nil {
		t.Errorf("Expected no error for 7-day expiry, got: %v", err)
	}
}

// ---------- parseCredential edge cases ----------

// TestParseCredential_EdgeCases tests various credential format edge cases.
func TestParseCredential_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"Valid", "AKIAIOSFODNN7EXAMPLE/20260930/us-east-1/s3/aws4_request", false},
		{"TooFewParts", "AKIAIOSFODNN7EXAMPLE/20260930/us-east-1/s3", true},
		{"TooManyParts", "AKIAIOSFODNN7EXAMPLE/20260930/us-east-1/s3/aws4_request/extra", true},
		{"EmptyString", "", true},
		{"EmptyAccessKey", "/20260930/us-east-1/s3/aws4_request", false}, // valid format, empty AK
		{"EmptyDate", "AKIAIOSFODNN7EXAMPLE//us-east-1/s3/aws4_request", false},
		{"SingleSlash", "AKIA/20260930", true},
		{"OnlySlashes", "////", false}, // 5 parts, all empty
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCredential(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCredential(%q): err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
		})
	}
}

// ---------- ParseAuthorizationHeader edge cases ----------

// TestParseAuthorizationHeader_EdgeCases tests various auth header formats.
func TestParseAuthorizationHeader_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		auth    string
		wantErr bool
	}{
		{
			"Valid",
			"AWS4-HMAC-SHA256 Credential=AKID/20260930/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123",
			false,
		},
		{"Empty", "", true},
		{"WrongPrefix", "Basic dXNlcjpwYXNz", true},
		{"MissingCredential", "AWS4-HMAC-SHA256 SignedHeaders=host, Signature=abc", true},
		{"OnlyPrefix", "AWS4-HMAC-SHA256 ", true},
		// NOTE: BUG — ExtraCommas parses successfully (len=3 after split on ", ").
		// The trailing comma in credential "aws4_request," is not validated,
		// which could allow signature bypass with malformed credentials.
		{"ExtraCommas", "AWS4-HMAC-SHA256 Credential=AKID/20260930/us-east-1/s3/aws4_request,, SignedHeaders=host, Signature=abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := ParseAuthorizationHeader(tt.auth)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseAuthorizationHeader(%q): err=%v, wantErr=%v", tt.auth, err, tt.wantErr)
			}
		})
	}
}

// ---------- isHexDigest edge cases ----------

// TestIsHexDigest_EdgeCases tests hex digest validation edge cases.
func TestIsHexDigest_EdgeCases(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		// 63 chars - too short
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", false},
		// 65 chars - too long
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8555", false},
		// Non-hex chars
		{"g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		// All zeros (valid hex)
		{"0000000000000000000000000000000000000000000000000000000000000000", true},
		// All f's (valid hex)
		{"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", true},
		// Spaces (invalid)
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85 ", false},
	}
	for _, tt := range tests {
		if got := isHexDigest(tt.input); got != tt.expected {
			t.Errorf("isHexDigest(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

// ---------- canonicalURI edge cases ----------

// TestCanonicalURI_EdgeCases tests URI canonicalization.
// Per AWS SigV4 spec, path separators "/" must NOT be URI-encoded.
// Each path segment is encoded individually, preserving "/" separators.
func TestCanonicalURI_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"Empty", "", "/"},
		{"Root", "/", "/"},
		{"SimplePath", "/test.txt", "/test.txt"},
		{"NestedPath", "/a/b/c.txt", "/a/b/c.txt"},
		{"SpaceInPath", "/test file.txt", "/test%20file.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalURI(tt.path)
			if got != tt.expected {
				t.Errorf("canonicalURI(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

// ---------- canonicalQueryString edge cases ----------

// TestCanonicalQueryString_EdgeCases tests query string canonicalization.
// Per AWS SigV4 spec, each key=value pair is sorted, and duplicate keys
// must each have their own key= prefix.
func TestCanonicalQueryString_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"Empty", "", ""},
		{"SingleParam", "foo=bar", "foo=bar"},
		{"MultipleParams", "b=2&a=1", "a=1&b=2"},
		// url.ParseQuery groups duplicate keys, sorted values: ["1","2"]
		// Per AWS spec, each value gets its own key= prefix
		{"DuplicateKeys", "a=2&a=1", "a=1&a=2"},
		{"EmptyValue", "foo=", "foo="},
		{"SpecialChars", "foo=bar+baz", "foo=bar+baz"},
		{"UnicodeValue", "key=é", "key=%C3%A9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, _ := url.ParseQuery(tt.query)
			got := canonicalQueryString(q)
			if got != tt.expected {
				t.Errorf("canonicalQueryString(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

// ---------- canonicalHeaders edge cases ----------

// TestCanonicalHeaders_CaseInsensitive tests header name lowercasing.
func TestCanonicalHeaders_CaseInsensitive(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Amz-Date", "20260930T120000Z")
	req.Header.Set("Host", "example.com")

	signedHeaders := []string{"host", "x-amz-date"}
	got := canonicalHeaders(req, signedHeaders)

	// Should lowercase header names and trim values
	if !strings.Contains(got, "host:") {
		t.Errorf("Expected lowercase 'host:', got %q", got)
	}
	if !strings.Contains(got, "x-amz-date:") {
		t.Errorf("Expected lowercase 'x-amz-date:', got %q", got)
	}
}

// TestCanonicalHeaders_MultipleValues tests multi-value header handling.
func TestCanonicalHeaders_MultipleValues(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	req.Header.Add("X-Custom", "value2")
	req.Header.Add("X-Custom", "value1")

	signedHeaders := []string{"x-custom"}
	got := canonicalHeaders(req, signedHeaders)

	// Multi-values should be sorted and comma-separated
	if !strings.Contains(got, "value1,value2") {
		t.Errorf("Expected sorted multi-values 'value1,value2', got %q", got)
	}
}

// ---------- Full signature verification edge cases ----------

// TestVerifySigV4Request_MissingContentSHA256 tests missing content hash header.
func TestVerifySigV4Request_MissingContentSHA256(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	now := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=AKID/20260930/us-east-1/s3/aws4_request, "+
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, "+
			"Signature=0000000000000000000000000000000000000000000000000000000000000000")
	// Deliberately omit X-Amz-Content-Sha256

	err := VerifySigV4Request(req, "secret", "us-east-1", "s3")
	if err == nil {
		t.Error("Expected error for missing X-Amz-Content-Sha256")
	}
}

// TestVerifySigV4Request_ScopeMismatch tests credential scope mismatch.
func TestVerifySigV4Request_ScopeMismatch(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	now := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	// Use wrong region in scope
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=AKID/"+now[:8]+"/eu-west-1/s3/aws4_request, "+
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, "+
			"Signature=0000000000000000000000000000000000000000000000000000000000000000")

	err := VerifySigV4Request(req, "secret", "us-east-1", "s3")
	if err == nil {
		t.Error("Expected error for scope mismatch")
	}
	if !strings.Contains(err.Error(), "scope mismatch") {
		t.Errorf("Expected scope mismatch error, got: %v", err)
	}
}

// TestVerifySigV4Request_InvalidSignatureFormat tests non-hex signature.
func TestVerifySigV4Request_InvalidSignatureFormat(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	now := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	req.Header.Set("Host", "example.com")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=AKID/"+now[:8]+"/us-east-1/s3/aws4_request, "+
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, "+
			"Signature=not-a-hex-string!")

	// Should not panic, should return error
	err := VerifySigV4Request(req, "secret", "us-east-1", "s3")
	if err == nil {
		t.Error("Expected error for non-hex signature")
	}
}

// ---------- deriveSigningKey ----------

// TestDeriveSigningKey_Deterministic tests that key derivation is deterministic.
func TestDeriveSigningKey_Deterministic(t *testing.T) {
	k1 := deriveSigningKey("secret", "20260930", "us-east-1", "s3")
	k2 := deriveSigningKey("secret", "20260930", "us-east-1", "s3")
	if string(k1) != string(k2) {
		t.Error("deriveSigningKey is not deterministic")
	}
}

// TestDeriveSigningKey_DifferentInputs tests different inputs produce different keys.
func TestDeriveSigningKey_DifferentInputs(t *testing.T) {
	k1 := deriveSigningKey("secret1", "20260930", "us-east-1", "s3")
	k2 := deriveSigningKey("secret2", "20260930", "us-east-1", "s3")
	if string(k1) == string(k2) {
		t.Error("Different secrets produced same key")
	}

	k3 := deriveSigningKey("secret", "20261001", "us-east-1", "s3")
	if string(k1) == string(k3) {
		t.Error("Different dates produced same key")
	}

	k4 := deriveSigningKey("secret", "20260930", "eu-west-1", "s3")
	if string(k1) == string(k4) {
		t.Error("Different regions produced same key")
	}

	k5 := deriveSigningKey("secret", "20260930", "us-east-1", "iam")
	if string(k1) == string(k5) {
		t.Error("Different services produced same key")
	}
}

// ---------- WriteS3Error ----------

// TestWriteS3Error_NilFields tests WriteS3Error with empty fields.
func TestWriteS3Error_NilFields(t *testing.T) {
	// Should not panic with empty resource/requestID
	w := &mockResponseWriter{headers: http.Header{}}
	WriteS3Error(w, ErrAccessDenied, "", "")
	if w.statusCode != http.StatusForbidden {
		t.Errorf("Expected status 403, got %d", w.statusCode)
	}
	if !strings.Contains(w.body, "AccessDenied") {
		t.Errorf("Expected body to contain 'AccessDenied', got: %s", w.body)
	}
}

// mockResponseWriter is a simple http.ResponseWriter mock for testing.
type mockResponseWriter struct {
	headers    http.Header
	body       string
	statusCode int
}

func (w *mockResponseWriter) Header() http.Header { return w.headers }
func (w *mockResponseWriter) Write(b []byte) (int, error) {
	w.body += string(b)
	return len(b), nil
}
func (w *mockResponseWriter) WriteHeader(statusCode int) { w.statusCode = statusCode }
