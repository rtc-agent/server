package rtcoss3

import (
	"net/http"
	"testing"
	"time"
)

// TestVerifyTimestamp tests timestamp validation.
func TestVerifyTimestamp(t *testing.T) {
	t.Run("ValidTimestamp", func(t *testing.T) {
		amzDate := time.Now().UTC().Format("20060102T150405Z")
		err := verifyTimestamp(amzDate, 15*time.Minute)
		if err != nil {
			t.Errorf("verifyTimestamp: %v", err)
		}
	})

	t.Run("ExpiredTimestamp", func(t *testing.T) {
		amzDate := time.Now().UTC().Add(-20 * time.Minute).Format("20060102T150405Z")
		err := verifyTimestamp(amzDate, 15*time.Minute)
		if err == nil {
			t.Error("Expected error for expired timestamp")
		}
		s3err, ok := err.(*S3Error)
		if !ok {
			t.Errorf("Expected S3Error, got %T", err)
		} else if s3err.Code != "RequestTimeTooSkewed" {
			t.Errorf("Expected RequestTimeTooSkewed, got %s", s3err.Code)
		}
	})

	t.Run("InvalidFormat", func(t *testing.T) {
		err := verifyTimestamp("invalid", 15*time.Minute)
		if err == nil {
			t.Error("Expected error for invalid format")
		}
	})
}

// TestIsHexDigest tests hex digest validation.
func TestIsHexDigest(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", true},
		{"UNSIGNED-PAYLOAD", false},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := isHexDigest(tt.input); got != tt.expected {
			t.Errorf("isHexDigest(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

// TestParseCredential tests credential parsing.
func TestParseCredential(t *testing.T) {
	cred, err := parseCredential("AKIAIOSFODNN7EXAMPLE/20260930/us-east-1/s3/aws4_request")
	if err != nil {
		t.Fatalf("parseCredential: %v", err)
	}
	if cred.AccessKeyID != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("AccessKeyID: got %q, want %q", cred.AccessKeyID, "AKIAIOSFODNN7EXAMPLE")
	}
	if cred.Date != "20260930" {
		t.Errorf("Date: got %q, want %q", cred.Date, "20260930")
	}
	if cred.Region != "us-east-1" {
		t.Errorf("Region: got %q, want %q", cred.Region, "us-east-1")
	}
	if cred.Service != "s3" {
		t.Errorf("Service: got %q, want %q", cred.Service, "s3")
	}
}

// TestCheckPresignedExpiry tests presigned URL expiry check.
func TestCheckPresignedExpiry(t *testing.T) {
	t.Run("ValidPresignedURL", func(t *testing.T) {
		amzDate := time.Now().UTC().Format("20060102T150405Z")
		err := checkPresignedExpiry(amzDate, "3600")
		if err != nil {
			t.Errorf("checkPresignedExpiry: %v", err)
		}
	})

	t.Run("ExpiredPresignedURL", func(t *testing.T) {
		amzDate := time.Now().UTC().Add(-2 * time.Hour).Format("20060102T150405Z")
		err := checkPresignedExpiry(amzDate, "3600")
		if err == nil {
			t.Error("Expected error for expired presigned URL")
		}
	})
}

// TestBuildCanonicalRequest tests canonical request building.
func TestBuildCanonicalRequest(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.com/test.txt?foo=bar", nil)
	req.Header.Set("Host", "example.com")
	req.Header.Set("X-Amz-Date", "20260930T120000Z")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalReq := buildCanonicalRequest(req, signedHeaders, "UNSIGNED-PAYLOAD")

	if canonicalReq == "" {
		t.Error("canonicalReq is empty")
	}
}

// TestDeriveSigningKey tests signing key derivation.
func TestDeriveSigningKey(t *testing.T) {
	key := deriveSigningKey("secret", "20260930", "us-east-1", "s3")
	if len(key) != 32 {
		t.Errorf("Expected 32-byte key, got %d", len(key))
	}
}
