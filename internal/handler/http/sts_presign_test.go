package httphandler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/config"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/usecase"
	rtcoss3 "github.com/rtc-agent/server/pkg/rtc-oss3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBackendDeleteObjects adds the missing DeleteObjects method to the mockBackend
// defined in oss3_test.go, satisfying the full rtcoss3.Backend interface.
func (m *mockBackend) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]rtcoss3.DeleteResult, error) {
	results := make([]rtcoss3.DeleteResult, len(keys))
	for i, key := range keys {
		fullKey := bucket + "/" + key
		delete(m.objects, fullKey)
		delete(m.meta, fullKey)
		results[i] = rtcoss3.DeleteResult{Key: key}
	}
	return results, nil
}

// newTestPresignHandler creates a STSPresignHandler with a mock backend for testing.
// The usecase is constructed with only the fields needed by GeneratePresignedURL.
func newTestPresignHandler(t *testing.T) *STSPresignHandler {
	t.Helper()

	backend := &mockBackend{
		objects: make(map[string][]byte),
		meta:    make(map[string]rtcoss3.ObjectMeta),
	}

	cfg := config.StorageConfig{
		MinIO: config.MinIOConfig{
			Bucket: "test-bucket",
		},
		Encryption: config.EncryptionConfig{
			SessionTokenKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}

	uc, err := usecase.NewOSS3Usecase(backend, nil, nil, nil, nil, nil, cfg)
	require.NoError(t, err)

	return NewSTSPresignHandler(uc, nil, false)
}

// doPresignRequest sends a POST /api/presigned-url request with the given body and context.
func doPresignRequest(t *testing.T, handler *STSPresignHandler, body interface{}, userID *uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}

	req := httptest.NewRequest(http.MethodPost, "/api/presigned-url", &buf)
	req.Header.Set("Content-Type", "application/json")

	ctx := req.Context()
	if userID != nil {
		ctx = contextx.WithClientInfo(ctx, *userID, "test-device")
	}
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.GeneratePresignedURL(rec, req)
	return rec
}

// TestSTSPresign_MissingUserID_Returns401 verifies that requests without a
// valid user ID in the context are rejected with 401 Unauthorized.
func TestSTSPresign_MissingUserID_Returns401(t *testing.T) {
	handler := newTestPresignHandler(t)

	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "put",
		Key:       "user-abc/file.txt",
	}, nil) // no user ID

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestSTSPresign_InvalidOperation_Returns400 verifies that unsupported
// operations (anything other than "put" or "get") are rejected with 400.
func TestSTSPresign_InvalidOperation_Returns400(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "delete",
		Key:       "user-00000000-0000-0000-0000-000000000001/file.txt",
	}, &uid)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "operation")
}

// TestSTSPresign_EmptyKey_Returns400 verifies that an empty key in the
// presign request is rejected with 400 Bad Request.
func TestSTSPresign_EmptyKey_Returns400(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "put",
		Key:       "",
	}, &uid)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "key")
}

// TestSTSPresign_KeyPrefixMismatch_Returns403 verifies that a user cannot
// obtain a presigned URL for a key owned by a different user; such requests
// must be rejected with 403 Forbidden.
func TestSTSPresign_KeyPrefixMismatch_Returns403(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// Key belongs to a different user
	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "put",
		Key:       "user-other-user/file.txt",
	}, &uid)

	assert.Equal(t, http.StatusForbidden, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "must start with")
}

// TestSTSPresign_ValidPut_Returns200 verifies that a well-formed "put"
// presign request returns 200 OK with a non-empty URL and a sensible
// expires_at timestamp.
func TestSTSPresign_ValidPut_Returns200(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "put",
		Key:       "user-00000000-0000-0000-0000-000000000001/file.txt",
		ExpiresIn: 3600,
	}, &uid)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp presignResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.URL)
	assert.NotEmpty(t, resp.ExpiresAt)

	// Verify expires_at is approximately 1h from now
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Hour), expiresAt, 5*time.Second)
}

// TestSTSPresign_ValidGet_Returns200 verifies that a well-formed "get"
// presign request returns 200 OK with a non-empty URL.
func TestSTSPresign_ValidGet_Returns200(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "get",
		Key:       "user-00000000-0000-0000-0000-000000000001/file.txt",
	}, &uid)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp presignResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.URL)
}

// TestSTSPresign_ExpiryExceeded_Returns403 verifies that requesting an
// expiry longer than the server-imposed maximum (7 days) is rejected with
// 403 Forbidden.
func TestSTSPresign_ExpiryExceeded_Returns403(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// Max is 7 days (604800s); request 8 days
	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "put",
		Key:       "user-00000000-0000-0000-0000-000000000001/file.txt",
		ExpiresIn: 8 * 24 * 3600, // 8 days
	}, &uid)

	assert.Equal(t, http.StatusForbidden, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "must not exceed")
}

// TestSTSPresign_DefaultExpiry_IsOneHour verifies that omitting the ExpiresIn
// field (or setting it to 0) results in a presigned URL with a default
// expiry of 1 hour.
func TestSTSPresign_DefaultExpiry_IsOneHour(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// expiresIn=0 triggers the default (1h)
	rec := doPresignRequest(t, handler, presignRequest{
		Operation: "put",
		Key:       "user-00000000-0000-0000-0000-000000000001/file.txt",
		ExpiresIn: 0,
	}, &uid)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp presignResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	require.NoError(t, err)
	// Default expiry should be approximately 1h from now
	assert.WithinDuration(t, time.Now().Add(time.Hour), expiresAt, 5*time.Second)
}

// TestSTSPresign_InvalidJSON_Returns400 verifies that a malformed JSON
// request body is rejected with 400 Bad Request.
func TestSTSPresign_InvalidJSON_Returns400(t *testing.T) {
	handler := newTestPresignHandler(t)
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	req := httptest.NewRequest(http.MethodPost, "/api/presigned-url", bytes.NewBufferString("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	ctx := contextx.WithClientInfo(req.Context(), uid, "test-device")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.GeneratePresignedURL(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
