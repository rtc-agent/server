package rtcoss3

import (
	"fmt"
	"net/http"
	"strings"
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

// TestE2ESignatureVerification tests end-to-end signature verification:
// construct a valid signed request -> verify passes, tamper -> verify fails.
// MEDIUM-25 fix.
func TestE2ESignatureVerification(t *testing.T) {
	secretKey := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	accessKey := "AKIAIOSFODNN7EXAMPLE"
	region := "us-east-1"
	service := "s3"
	now := time.Now().UTC()
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	t.Run("ValidSignature_Passes", func(t *testing.T) {
		// Build a simple GET request
		req, _ := http.NewRequest("GET", "/rtc-agent/user-123/test.txt", nil)
		req.Host = "localhost:9000"
		req.Header.Set("X-Amz-Date", amzDate)
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

		// Compute signature manually
		scope := date + "/" + region + "/" + service + "/aws4_request"
		signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}

		canonicalReq := buildCanonicalRequest(req, signedHeaders)
		stringToSign := buildStringToSign(amzDate, scope, canonicalReq)
		signingKey := deriveSigningKey(secretKey, date, region, service)
		signature := fmt.Sprintf("%x", hmacSHA256(signingKey, []byte(stringToSign)))

		authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
			accessKey, scope, "host;x-amz-content-sha256;x-amz-date", signature)
		req.Header.Set("Authorization", authHeader)

		// Verify should pass
		err := VerifySigV4Request(req, secretKey, region, service)
		if err != nil {
			t.Errorf("Valid signature should pass, got: %v", err)
		}
	})

	t.Run("TamperedSignature_Fails", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/rtc-agent/user-123/test.txt", nil)
		req.Host = "localhost:9000"
		req.Header.Set("X-Amz-Date", amzDate)
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

		scope := date + "/" + region + "/" + service + "/aws4_request"
		signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}

		canonicalReq := buildCanonicalRequest(req, signedHeaders)
		stringToSign := buildStringToSign(amzDate, scope, canonicalReq)
		signingKey := deriveSigningKey(secretKey, date, region, service)
		signature := fmt.Sprintf("%x", hmacSHA256(signingKey, []byte(stringToSign)))

		authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
			accessKey, scope, "host;x-amz-content-sha256;x-amz-date", signature)
		req.Header.Set("Authorization", authHeader)

		// Tamper: change the path after signing
		req.URL.Path = "/rtc-agent/user-123/other.txt"

		err := VerifySigV4Request(req, secretKey, region, service)
		if err == nil {
			t.Error("Tampered request should fail verification")
		}
	})

	t.Run("TamperedHeader_Fails", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/rtc-agent/user-123/test.txt", nil)
		req.Host = "localhost:9000"
		req.Header.Set("X-Amz-Date", amzDate)
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

		scope := date + "/" + region + "/" + service + "/aws4_request"
		signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}

		canonicalReq := buildCanonicalRequest(req, signedHeaders)
		stringToSign := buildStringToSign(amzDate, scope, canonicalReq)
		signingKey := deriveSigningKey(secretKey, date, region, service)
		signature := fmt.Sprintf("%x", hmacSHA256(signingKey, []byte(stringToSign)))

		authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
			accessKey, scope, "host;x-amz-content-sha256;x-amz-date", signature)
		req.Header.Set("Authorization", authHeader)

		// Tamper: change a signed header after signing
		req.Header.Set("X-Amz-Date", "20200101T000000Z")

		err := VerifySigV4Request(req, secretKey, region, service)
		if err == nil {
			t.Error("Tampered header should fail verification")
		}
	})

	t.Run("WrongSecretKey_Fails", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/rtc-agent/user-123/test.txt", nil)
		req.Host = "localhost:9000"
		req.Header.Set("X-Amz-Date", amzDate)
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

		scope := date + "/" + region + "/" + service + "/aws4_request"
		signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}

		canonicalReq := buildCanonicalRequest(req, signedHeaders)
		stringToSign := buildStringToSign(amzDate, scope, canonicalReq)
		// Sign with wrong key
		signingKey := deriveSigningKey("wrongkey", date, region, service)
		signature := fmt.Sprintf("%x", hmacSHA256(signingKey, []byte(stringToSign)))

		authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
			accessKey, scope, "host;x-amz-content-sha256;x-amz-date", signature)
		req.Header.Set("Authorization", authHeader)

		// Verify with correct key should fail
		err := VerifySigV4Request(req, secretKey, region, service)
		if err == nil {
			t.Error("Wrong secret key should fail verification")
		}
	})
}

// TestSignS3Request tests the exported SignS3Request helper.
// It signs a request and then verifies it with VerifySigV4Request.
func TestSignS3Request(t *testing.T) {
	secretKey := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	accessKey := "AKIAIOSFODNN7EXAMPLE"
	region := "us-east-1"

	t.Run("GET request", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/rtc-agent/user-123/test.txt", nil)
		req.Host = "localhost:9000"

		SignS3Request(req, accessKey, secretKey, "", region)

		// Verify the signed request
		err := VerifySigV4Request(req, secretKey, region, "s3")
		if err != nil {
			t.Errorf("SignS3Request produced invalid signature: %v", err)
		}

		// Verify Authorization header is set
		auth := req.Header.Get("Authorization")
		if auth == "" {
			t.Error("Authorization header not set")
		}
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
			t.Errorf("Authorization header should start with AWS4-HMAC-SHA256, got: %s", auth)
		}

		// Verify required headers are set
		if req.Header.Get("X-Amz-Date") == "" {
			t.Error("X-Amz-Date header not set")
		}
		if req.Header.Get("X-Amz-Content-Sha256") != "UNSIGNED-PAYLOAD" {
			t.Errorf("X-Amz-Content-Sha256 should be UNSIGNED-PAYLOAD, got: %s", req.Header.Get("X-Amz-Content-Sha256"))
		}
	})

	t.Run("PUT request with body", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", "/rtc-agent/user-123/test.txt", strings.NewReader("hello world"))
		req.Host = "localhost:9000"

		SignS3Request(req, accessKey, secretKey, "", region)

		err := VerifySigV4Request(req, secretKey, region, "s3")
		if err != nil {
			t.Errorf("SignS3Request produced invalid signature for PUT: %v", err)
		}
	})

	t.Run("POST with query params", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "/rtc-agent?delete", nil)
		req.Host = "localhost:9000"

		SignS3Request(req, accessKey, secretKey, "", region)

		err := VerifySigV4Request(req, secretKey, region, "s3")
		if err != nil {
			t.Errorf("SignS3Request produced invalid signature for POST with query: %v", err)
		}
	})

	t.Run("wrong secret key fails verification", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/rtc-agent/user-123/test.txt", nil)
		req.Host = "localhost:9000"

		SignS3Request(req, accessKey, secretKey, "", region)

		// Verify with wrong secret key should fail
		err := VerifySigV4Request(req, "wrong-secret-key", region, "s3")
		if err == nil {
			t.Error("Verification with wrong secret key should fail")
		}
	})
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
	canonicalReq := buildCanonicalRequest(req, signedHeaders)

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
