package httphandler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getPresignedURL requests a presigned URL from the STS endpoint.
func getPresignedURL(t *testing.T, accessToken, serverURL, operation, key string, expiresIn int64) *presignResponse {
	t.Helper()

	reqBody := presignRequest{
		Operation: operation,
		Key:       key,
		ExpiresIn: expiresIn,
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/presigned-url", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "presign request failed: %s", string(bodyBytes))

	var result presignResponse
	require.NoError(t, json.Unmarshal(bodyBytes, &result))
	require.NotEmpty(t, result.URL)
	require.NotEmpty(t, result.ExpiresAt)

	return &result
}

// TestPresignedURL_PutGet_Integration tests the complete end-to-end flow:
// 1. Get access token and S3 credentials
// 2. Request a presigned URL for PUT
// 3. Upload a file using the presigned URL
// 4. Request a presigned URL for GET
// 5. Download the file using the presigned URL
// 6. Verify the downloaded content matches the uploaded content
func TestPresignedURL_PutGet_Integration(t *testing.T) {
	_, userID, serverURL, _ := setupIntegrationTest(t)

	// We need an access token, not just S3 credentials.
	// Get it from the refresh token.
	refreshToken := os.Getenv("RTC_OSS3_REFRESH_TOKEN")
	if refreshToken == "" {
		t.Skip("RTC_OSS3_REFRESH_TOKEN not set")
	}
	accessToken := getAccessToken(t, refreshToken, serverURL)

	// Generate a valid key for upload
	key := generateValidKey(userID, "txt")
	uploadedContent := []byte("Hello from presigned URL integration test!")

	// Step 1: Get presigned URL for PUT
	putPresign := getPresignedURL(t, accessToken, serverURL, "put", key, 3600)
	t.Logf("Presigned PUT URL: %s", putPresign.URL)

	// Step 2: Upload file using presigned URL
	uploadReq, err := http.NewRequest(http.MethodPut, putPresign.URL, bytes.NewReader(uploadedContent))
	require.NoError(t, err)
	uploadReq.Header.Set("Content-Type", "text/plain")

	uploadResp, err := http.DefaultClient.Do(uploadReq)
	require.NoError(t, err)
	defer uploadResp.Body.Close()

	uploadBody, _ := io.ReadAll(uploadResp.Body)
	assert.Equal(t, http.StatusOK, uploadResp.StatusCode, "upload via presigned URL failed: %s", string(uploadBody))
	t.Logf("Successfully uploaded file via presigned URL")

	// Step 3: Get presigned URL for GET
	getPresign := getPresignedURL(t, accessToken, serverURL, "get", key, 3600)
	t.Logf("Presigned GET URL: %s", getPresign.URL)

	// Step 4: Download file using presigned URL
	downloadReq, err := http.NewRequest(http.MethodGet, getPresign.URL, nil)
	require.NoError(t, err)

	downloadResp, err := http.DefaultClient.Do(downloadReq)
	require.NoError(t, err)
	defer downloadResp.Body.Close()

	downloadedContent, err := io.ReadAll(downloadResp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, downloadResp.StatusCode)
	assert.Equal(t, uploadedContent, downloadedContent, "downloaded content does not match uploaded content")
	t.Logf("Successfully downloaded and verified file via presigned URL")

	// Note: cleanup is optional as the test uses unique keys per run
}

// TestPresignedURL_KeyValidation_Integration tests the key validation behavior
// when requesting presigned URLs. The presign API validates both user prefix (403)
// and strict key format (400) before generating a URL.
func TestPresignedURL_KeyValidation_Integration(t *testing.T) {
	_, userID, serverURL, _ := setupIntegrationTest(t)

	refreshToken := os.Getenv("RTC_OSS3_REFRESH_TOKEN")
	if refreshToken == "" {
		t.Skip("RTC_OSS3_REFRESH_TOKEN not set")
	}
	accessToken := getAccessToken(t, refreshToken, serverURL)

	// Test 1: Key belonging to another user should return 403
	// Use valid hex chars (a-f, 0-9) in the MD5 portion
	otherUserID := "99999999-9999-9999-9999-999999999999"
	crossUserKey := "user-" + otherUserID + "/a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4.txt"

	reqBody := presignRequest{
		Operation: "get",
		Key:       crossUserKey,
		ExpiresIn: 3600,
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/presigned-url", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "expected 403 for cross-user key")
	bodyBytes, _ := io.ReadAll(resp.Body)
	t.Logf("Cross-user key rejected: %s", string(bodyBytes))

	// Test 2: Empty key should return 400
	reqBody = presignRequest{
		Operation: "put",
		Key:       "",
		ExpiresIn: 3600,
	}
	body, _ = json.Marshal(reqBody)
	req, _ = http.NewRequest(http.MethodPost, serverURL+"/api/presigned-url", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "expected 400 for empty key")
	bodyBytes, _ = io.ReadAll(resp.Body)
	t.Logf("Empty key rejected: %s", string(bodyBytes))

	// Test 3: Valid user prefix but non-standard key format should now return 400
	// (key format is validated at presign time, not just at S3 endpoint)
	nonStandardKey := "user-" + userID + "/non-standard-key-format.txt"
	reqBody = presignRequest{
		Operation: "put",
		Key:       nonStandardKey,
		ExpiresIn: 3600,
	}
	body, _ = json.Marshal(reqBody)
	req, _ = http.NewRequest(http.MethodPost, serverURL+"/api/presigned-url", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Key format validation now happens at presign time
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "expected 400 for non-standard key format")
	bodyBytes, _ = io.ReadAll(resp.Body)
	t.Logf("Non-standard key rejected at presign time: %s", string(bodyBytes))
}
