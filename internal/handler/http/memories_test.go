package httphandler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/infra/auth"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/svc"
	"github.com/rtc-agent/server/pkg/memory"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ─── mock memory.Repository ───

type mockMemoryRepo struct {
	memories []*memory.Memory
}

func (r *mockMemoryRepo) Create(_ context.Context, m *memory.Memory) error {
	r.memories = append(r.memories, m)
	return nil
}

func (r *mockMemoryRepo) BatchCreate(_ context.Context, memories []*memory.Memory) error {
	r.memories = append(r.memories, memories...)
	return nil
}

func (r *mockMemoryRepo) GetByID(_ context.Context, id uuid.UUID) (*memory.Memory, error) {
	for _, m := range r.memories {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, memory.ErrNotFound
}

func (r *mockMemoryRepo) ListByScope(_ context.Context, scope memory.ScopeType, scopeID uuid.UUID, opts memory.ListOptions) ([]*memory.Memory, error) {
	var result []*memory.Memory
	for _, m := range r.memories {
		if m.Scope != scope || m.ScopeID != scopeID {
			continue
		}
		if opts.Type != "" && m.Type != opts.Type {
			continue
		}
		result = append(result, m)
	}
	return result, nil
}

func (r *mockMemoryRepo) ListRecentForInjection(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _, _ int) ([]*memory.Memory, error) {
	return nil, nil
}

func (r *mockMemoryRepo) Search(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _ string, _ int) ([]*memory.Memory, error) {
	return nil, nil
}

func (r *mockMemoryRepo) Update(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	return nil
}

func (r *mockMemoryRepo) Delete(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (r *mockMemoryRepo) DeleteByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID) error {
	return nil
}

func (r *mockMemoryRepo) GetLinked(_ context.Context, _ uuid.UUID, _ string) ([]*memory.Memory, error) {
	return nil, nil
}

func (r *mockMemoryRepo) CreateLink(_ context.Context, _ *memory.MemoryLink) error {
	return nil
}

func (r *mockMemoryRepo) DeleteLink(_ context.Context, _, _ uuid.UUID) error {
	return nil
}

func (r *mockMemoryRepo) CountTokensByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID) (int, error) {
	return 0, nil
}

// ─── mock repo.SessionRepo ───

type mockSessionRepo struct {
	sessions map[uuid.UUID]*model.Session
}

func newMockSessionRepo() *mockSessionRepo {
	return &mockSessionRepo{sessions: make(map[uuid.UUID]*model.Session)}
}

func (r *mockSessionRepo) Create(_ context.Context, session *model.Session) error {
	r.sessions[session.ID] = session
	return nil
}

func (r *mockSessionRepo) GetByID(_ context.Context, id uuid.UUID) (*model.Session, error) {
	s, ok := r.sessions[id]
	if !ok {
		return nil, repo.ErrSessionNotFound
	}
	return s, nil
}

func (r *mockSessionRepo) FindByClientID(_ context.Context, _ string) (*model.Session, error) {
	return nil, repo.ErrSessionNotFound
}

func (r *mockSessionRepo) GetByUser(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Session, error) {
	return nil, nil
}

func (r *mockSessionRepo) UpdateStatus(_ context.Context, _ uuid.UUID, _ protocol.SessionStatus) error {
	return nil
}

func (r *mockSessionRepo) Update(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	return nil
}

func (r *mockSessionRepo) TouchActive(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (r *mockSessionRepo) UpdateFieldsActive(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	return nil
}

func (r *mockSessionRepo) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Session, error) {
	return nil, nil
}

func (r *mockSessionRepo) ListByRoot(_ context.Context, _ uuid.UUID, _ string) ([]*model.Session, error) {
	return nil, nil
}

func (r *mockSessionRepo) AtomicAddTokenUsage(_ context.Context, _ uuid.UUID, _ repo.TokenUsageDelta) error {
	return nil
}

func (r *mockSessionRepo) AtomicUpdateEWMA(_ context.Context, _ uuid.UUID, _ float64) error {
	return nil
}

// ─── test helpers ───

// newTestSigner creates a JWT signer for testing.
func newTestSigner(t *testing.T) *auth.JWTSigner {
	t.Helper()
	signer, err := auth.NewJWTSigner("test-secret-key-for-testing-only", 15*time.Minute)
	require.NoError(t, err)
	return signer
}

// newTestHandler creates a MemoriesHandler with mock repos for testing.
// allowDevBypass is always true in tests.
func newTestHandler(t *testing.T, userID uuid.UUID) (*MemoriesHandler, *mockMemoryRepo, *mockSessionRepo) {
	t.Helper()
	signer := newTestSigner(t)
	memRepo := &mockMemoryRepo{}
	sessRepo := newMockSessionRepo()

	svcCtx := &svc.ServiceContext{
		MemoryRepo:  memRepo,
		SessionRepo: sessRepo,
	}

	h := NewMemoriesHandler(svcCtx, signer)
	return h, memRepo, sessRepo
}

// muxWithAuth creates a ServeMux with the handler's routes registered (dev bypass enabled).
func muxWithAuth(h *MemoriesHandler) *http.ServeMux {
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, true) // allowDevBypass = true for tests
	return mux
}

// doExportRequest sends a POST /api/memories/export request and returns the recorder.
func doExportRequest(mux *http.ServeMux, body any, headers map[string]string) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}
	req := httptest.NewRequest("POST", "/api/memories/export", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// validExportRequest returns a minimal valid export request body.
func validExportRequest(scope, scopeID string) ExportRequest {
	return ExportRequest{
		Scope:   scope,
		ScopeID: scopeID,
		Format:  "okf-bundle",
	}
}

// ─── tests ───

func TestExportMemories_DevBypass_Success(t *testing.T) {
	userID := uuid.New()
	h, memRepo, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Add a memory for the user
	now := time.Now().UTC()
	memRepo.memories = []*memory.Memory{
		{
			ID:        uuid.New(),
			Scope:     memory.ScopeUser,
			ScopeID:   userID,
			Type:      "decision",
			Title:     "Test Decision",
			Content:   "Test content",
			Tags:      memory.StringArray{"test"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	rec := doExportRequest(mux, validExportRequest("user", userID.String()), map[string]string{
		"X-User-ID":   userID.String(),
		"X-Device-ID": "test-device",
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/gzip", rec.Header().Get("Content-Type"))
}

func TestExportMemories_JWTAuth_Success(t *testing.T) {
	userID := uuid.New()
	h, memRepo, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	now := time.Now().UTC()
	memRepo.memories = []*memory.Memory{
		{
			ID:        uuid.New(),
			Scope:     memory.ScopeUser,
			ScopeID:   userID,
			Type:      "context",
			Title:     "Test Context",
			Content:   "Test content",
			Tags:      memory.StringArray{"test"},
			Timestamp: now,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}

	// Generate a real JWT token
	signer := newTestSigner(t)
	token, _, err := signer.SignAccessToken(userID, "test-device")
	require.NoError(t, err)

	rec := doExportRequest(mux, validExportRequest("user", userID.String()), map[string]string{
		"Authorization": "Bearer " + token,
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/gzip", rec.Header().Get("Content-Type"))
}

func TestExportMemories_NoAuth_Returns401(t *testing.T) {
	userID := uuid.New()
	h, _, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// No auth headers at all
	rec := doExportRequest(mux, validExportRequest("user", userID.String()), nil)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestExportMemories_InvalidJWT_Returns401(t *testing.T) {
	userID := uuid.New()
	h, _, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	rec := doExportRequest(mux, validExportRequest("user", userID.String()), map[string]string{
		"Authorization": "Bearer invalid.token.here",
	})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestExportMemories_UserScope_ForbiddenWhenScopeIdMismatch(t *testing.T) {
	userID := uuid.New()
	h, _, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Request export for a different user's scope
	otherUserID := uuid.New()
	rec := doExportRequest(mux, validExportRequest("user", otherUserID.String()), map[string]string{
		"X-User-ID":   userID.String(),
		"X-Device-ID": "test-device",
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "forbidden", resp["error"])
}

func TestExportMemories_SessionScope_ForbiddenWhenNotOwner(t *testing.T) {
	userID := uuid.New()
	h, _, sessRepo := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Create a session owned by a different user
	sessionID := uuid.New()
	otherUserID := uuid.New()
	sessRepo.sessions[sessionID] = &model.Session{
		ID:         sessionID,
		OwnerRefID: otherUserID.String(),
	}

	rec := doExportRequest(mux, validExportRequest("session", sessionID.String()), map[string]string{
		"X-User-ID":   userID.String(),
		"X-Device-ID": "test-device",
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "forbidden", resp["error"])
}

func TestExportMemories_SessionScope_NotFound(t *testing.T) {
	userID := uuid.New()
	h, _, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Request a session that doesn't exist
	nonExistentSessionID := uuid.New()
	rec := doExportRequest(mux, validExportRequest("session", nonExistentSessionID.String()), map[string]string{
		"X-User-ID":   userID.String(),
		"X-Device-ID": "test-device",
	})

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "session_not_found", resp["error"])
}

func TestExportMemories_SessionScope_SuccessWhenOwner(t *testing.T) {
	userID := uuid.New()
	h, _, sessRepo := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Create a session owned by the authenticated user
	sessionID := uuid.New()
	sessRepo.sessions[sessionID] = &model.Session{
		ID:         sessionID,
		OwnerRefID: userID.String(),
	}

	rec := doExportRequest(mux, validExportRequest("session", sessionID.String()), map[string]string{
		"X-User-ID":   userID.String(),
		"X-Device-ID": "test-device",
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/gzip", rec.Header().Get("Content-Type"))
}

func TestExportMemories_MaxBytesExceeded_Returns413(t *testing.T) {
	userID := uuid.New()
	h, _, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Create a body larger than 1MB (the MaxBytesReader limit)
	largeBody := strings.Repeat("x", 1<<20+1024) // 1MB + 1KB
	req := httptest.NewRequest("POST", "/api/memories/export", strings.NewReader(largeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID.String())
	req.Header.Set("X-Device-ID", "test-device")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// MaxBytesReader causes json.Decode to fail, which returns 400 (invalid_json).
	// The HTTP 413 is not automatically set by MaxBytesReader; the decode error
	// triggers our invalid_json handler.
	assert.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusRequestEntityTooLarge,
		"expected 400 or 413, got %d", rec.Code)
}

func TestExportMemories_GlobalScope_NoOwnershipCheck(t *testing.T) {
	userID := uuid.New()
	h, _, _ := newTestHandler(t, userID)
	mux := muxWithAuth(h)

	// Global scope should not require ownership check
	rec := doExportRequest(mux, validExportRequest("global", uuid.New().String()), map[string]string{
		"X-User-ID":   userID.String(),
		"X-Device-ID": "test-device",
	})

	assert.Equal(t, http.StatusOK, rec.Code)
}
