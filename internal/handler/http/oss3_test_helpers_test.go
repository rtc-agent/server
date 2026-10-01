package httphandler

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
)

// s3Creds holds temporary S3 credentials obtained from the STS endpoint.
type s3Creds struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	ExpiresAt       string `json:"expires_at"`
}

// getAccessToken exchanges a refresh token for an access token via POST /oauth2/refresh.
// Returns the access token string. Calls t.Fatal on failure.
func getAccessToken(t *testing.T, refreshToken string, serverURL string) string {
	t.Helper()

	reqBody := map[string]string{
		"refresh_token": refreshToken,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal refresh request: %v", err)
	}

	resp, err := http.Post(serverURL+"/oauth2/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /oauth2/refresh: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /oauth2/refresh: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("refresh response missing access_token")
	}
	return result.AccessToken
}

// getS3Credentials obtains temporary S3 credentials using a JWT access token.
// Calls POST /api/credentials/temporary with the Bearer token.
func getS3Credentials(t *testing.T, accessToken string, serverURL string) *s3Creds {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/credentials/temporary", nil)
	if err != nil {
		t.Fatalf("create credentials request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/credentials/temporary: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/credentials/temporary: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var creds s3Creds
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		t.Fatalf("decode credentials response: %v", err)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		t.Fatal("credentials response missing access_key_id or secret_access_key")
	}
	return &creds
}

// setupIntegrationTest performs the full auth setup for integration tests:
// 1. Get access token from refresh token
// 2. Extract user ID from JWT
// 3. Get temporary S3 credentials
// Returns the S3 credentials, user ID, server URL, and bucket.
// Skips the test if RTC_OSS3_REFRESH_TOKEN is not set.
func setupIntegrationTest(t *testing.T) (creds *s3Creds, userID, serverURL, bucket string) {
	t.Helper()

	refreshToken := os.Getenv("RTC_OSS3_REFRESH_TOKEN")
	if refreshToken == "" {
		t.Skip("Skipping: RTC_OSS3_REFRESH_TOKEN not set")
	}

	serverURL = os.Getenv("RTC_OSS3_SERVER_URL")
	if serverURL == "" {
		serverURL = "http://localhost:8080"
	}
	bucket = os.Getenv("RTC_OSS3_BUCKET")
	if bucket == "" {
		bucket = "rtc-agent"
	}

	accessToken := getAccessToken(t, refreshToken, serverURL)
	userID = extractUserIDFromJWT(t, accessToken)
	creds = getS3Credentials(t, accessToken, serverURL)
	return creds, userID, serverURL, bucket
}

// extractUserIDFromJWT extracts the user_id claim from a JWT access token.
func extractUserIDFromJWT(t *testing.T, accessToken string) string {
	t.Helper()

	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}

	var claims struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal JWT claims: %v", err)
	}

	if claims.UserID == "" {
		t.Fatal("JWT missing user_id claim")
	}

	return claims.UserID
}

// newSignedS3Request creates an S3-signed HTTP request.
func newSignedS3Request(t *testing.T, method, path string, body io.Reader, creds *s3Creds, serverURL string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(method, serverURL+path, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	// Set Host header for signing
	parsedURL, _ := url.Parse(serverURL)
	if parsedURL != nil {
		req.Host = parsedURL.Host
	}

	// Sign with SigV4 (pass empty session token — server verifies via stored credential,
	// not via X-Amz-Security-Token header, so including it in signed headers causes mismatch)
	rtcoss3.SignS3Request(req, creds.AccessKeyID, creds.SecretAccessKey, "", "us-east-1")

	return req
}

// generateValidKey generates a valid S3 key in the format: user-{uuid}/{md5}.{ext}
// The userID is embedded in the key for ownership validation.
func generateValidKey(userID string, ext string) string {
	// Generate a deterministic MD5 based on userID and timestamp
	data := fmt.Sprintf("%s-%d", userID, time.Now().UnixNano())
	hash := fmt.Sprintf("%x", md5.Sum([]byte(data)))
	return fmt.Sprintf("user-%s/%s.%s", userID, hash, ext)
}

// testUserID returns a fixed UUID string for testing.
func testUserID() string {
	return "00000000-0000-0000-0000-000000000001"
}
