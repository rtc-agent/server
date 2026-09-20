// Package rpchandler — OpenSession handler unit tests.
package rpchandler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// Mock SessionRepo (implements all 14 methods of repo.SessionRepo)
// ---------------------------------------------------------------------------

type mockSessionRepo struct {
	getByIDFunc      func(ctx context.Context, id uuid.UUID) (*model.Session, error)
	updateStatusFunc func(ctx context.Context, id uuid.UUID, status protocol.SessionStatus) error
	updateFunc       func(ctx context.Context, id uuid.UUID, fields map[string]any) error

	// Track calls for assertions.
	updateStatusCalls []updateStatusCall
	updateCalls       []updateCall
}

type updateStatusCall struct {
	ID     uuid.UUID
	Status protocol.SessionStatus
}

type updateCall struct {
	ID     uuid.UUID
	Fields map[string]any
}

func (m *mockSessionRepo) Create(_ context.Context, _ *model.Session) error { return nil }

func (m *mockSessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Session, error) {
	if m.getByIDFunc != nil {
		return m.getByIDFunc(ctx, id)
	}
	return nil, repo.ErrSessionNotFound
}

func (m *mockSessionRepo) FindByClientID(_ context.Context, _ string) (*model.Session, error) {
	return nil, nil
}

func (m *mockSessionRepo) GetByUser(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Session, error) {
	return nil, nil
}

func (m *mockSessionRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status protocol.SessionStatus) error {
	m.updateStatusCalls = append(m.updateStatusCalls, updateStatusCall{ID: id, Status: status})
	if m.updateStatusFunc != nil {
		return m.updateStatusFunc(ctx, id, status)
	}
	return nil
}

func (m *mockSessionRepo) Update(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	m.updateCalls = append(m.updateCalls, updateCall{ID: id, Fields: fields})
	if m.updateFunc != nil {
		return m.updateFunc(ctx, id, fields)
	}
	return nil
}

func (m *mockSessionRepo) TouchActive(_ context.Context, _ uuid.UUID) error { return nil }

func (m *mockSessionRepo) UpdateFieldsActive(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	return nil
}

func (m *mockSessionRepo) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Session, error) {
	return nil, nil
}

func (m *mockSessionRepo) ListByRoot(_ context.Context, _ uuid.UUID, _ string) ([]*model.Session, error) {
	return nil, nil
}

func (m *mockSessionRepo) FindActiveByParent(_ context.Context, _ uuid.UUID) ([]*model.Session, error) {
	return nil, nil
}

func (m *mockSessionRepo) AtomicAddTokenUsage(_ context.Context, _ uuid.UUID, _ repo.TokenUsageDelta) error {
	return nil
}

func (m *mockSessionRepo) AtomicUpdateEWMA(_ context.Context, _ uuid.UUID, _ float64) error {
	return nil
}

// ---------------------------------------------------------------------------
// Mock Publisher (implements usecase.Publisher)
// ---------------------------------------------------------------------------

type mockPublisher struct {
	// runAndPublishFunc simulates RunAndPublish: calls fn with ctx to execute
	// the transaction callback, then returns nil updates (sufficient for tests).
	runAndPublishFunc func(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error)
	publishCalled     bool
}

func (m *mockPublisher) Publish(_ context.Context, _ ...updates.UpdatePublishItem) ([]*protocol.Update, error) {
	m.publishCalled = true
	return nil, nil
}

func (m *mockPublisher) RunAndPublish(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
	if m.runAndPublishFunc != nil {
		return m.runAndPublishFunc(ctx, fn)
	}
	// Default: execute fn with ctx (simulates transaction), return nil updates.
	_, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (m *mockPublisher) ResolveMessageContent(_ *model.Message) string { return "" }

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestOpenSessionHandler creates a Handler wired with mock repos/publisher.
// Returns the handler, the mock session repo, and the mock publisher.
func newTestOpenSessionHandler(t *testing.T, sessionRepo *mockSessionRepo, publisher *mockPublisher) *Handler {
	t.Helper()

	usecaseDeps := &usecase.Dependencies{
		SessionRepo:     sessionRepo,
		UpdatePublisher: publisher,
	}

	deps := &Dependencies{
		Deps:        usecaseDeps,
		SessionRepo: sessionRepo,
	}

	return &Handler{deps: deps}
}

// userCtx returns a context with the given userID injected.
func userCtx(t *testing.T, userID uuid.UUID) context.Context {
	t.Helper()
	return contextx.WithClientInfo(context.Background(), userID, "test-device")
}

// closedSession builds a closed session owned by userID.
func closedSession(t *testing.T, ownerID uuid.UUID) *model.Session {
	t.Helper()
	now := time.Now()
	return &model.Session{
		ID:         uuid.New(),
		OwnerKind:  "user",
		OwnerRefID: ownerID.String(),
		Status:     string(protocol.SessionStatusClosed),
		ClosedAt:   &now,
	}
}

// assertAPIError checks that err is an *APIError with the expected Code.
func assertAPIError(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", wantCode)
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != wantCode {
		t.Errorf("APIError.Code = %q, want %q (message: %s)", apiErr.Code, wantCode, apiErr.Message)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOpenSession_Success_ReopenClosedSession(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	session := closedSession(t, userID)

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == session.ID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}

	publisher := &mockPublisher{}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: session.ID.String()}

	resp, err := h.OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession returned unexpected error: %v", err)
	}
	if !resp.Result.Success {
		t.Error("expected Result.Success = true")
	}

	// Verify UpdateStatus was called with idle.
	if len(sessionRepo.updateStatusCalls) != 1 {
		t.Fatalf("UpdateStatus called %d times, want 1", len(sessionRepo.updateStatusCalls))
	}
	if sessionRepo.updateStatusCalls[0].Status != protocol.SessionStatusIdle {
		t.Errorf("UpdateStatus status = %q, want %q",
			sessionRepo.updateStatusCalls[0].Status, protocol.SessionStatusIdle)
	}
	if sessionRepo.updateStatusCalls[0].ID != session.ID {
		t.Errorf("UpdateStatus id = %s, want %s",
			sessionRepo.updateStatusCalls[0].ID, session.ID)
	}

	// Verify Update was called to clear closed_at.
	if len(sessionRepo.updateCalls) != 1 {
		t.Fatalf("Update called %d times, want 1", len(sessionRepo.updateCalls))
	}
	fields := sessionRepo.updateCalls[0].Fields
	if _, ok := fields["closed_at"]; !ok {
		t.Error("Update fields missing 'closed_at'")
	}
	if fields["closed_at"] != nil {
		t.Errorf("Update closed_at = %v, want nil", fields["closed_at"])
	}
	if sessionRepo.updateCalls[0].ID != session.ID {
		t.Errorf("Update id = %s, want %s", sessionRepo.updateCalls[0].ID, session.ID)
	}
}

func TestOpenSession_Idempotent_AlreadyIdle(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	session := &model.Session{
		ID:         uuid.New(),
		OwnerKind:  "user",
		OwnerRefID: userID.String(),
		Status:     string(protocol.SessionStatusIdle),
	}

	runAndPublishCalled := false
	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == session.ID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}
	publisher := &mockPublisher{
		runAndPublishFunc: func(_ context.Context, _ func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
			runAndPublishCalled = true
			return nil, nil
		},
	}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: session.ID.String()}

	resp, err := h.OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession returned unexpected error: %v", err)
	}
	if !resp.Result.Success {
		t.Error("expected Result.Success = true")
	}
	if runAndPublishCalled {
		t.Error("RunAndPublish should not be called for idle session")
	}
	if len(sessionRepo.updateStatusCalls) != 0 {
		t.Errorf("UpdateStatus should not be called, got %d calls", len(sessionRepo.updateStatusCalls))
	}
}

func TestOpenSession_Idempotent_Active(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	session := &model.Session{
		ID:         uuid.New(),
		OwnerKind:  "user",
		OwnerRefID: userID.String(),
		Status:     string(protocol.SessionStatusActive),
	}

	runAndPublishCalled := false
	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == session.ID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}
	publisher := &mockPublisher{
		runAndPublishFunc: func(_ context.Context, _ func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
			runAndPublishCalled = true
			return nil, nil
		},
	}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: session.ID.String()}

	resp, err := h.OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession returned unexpected error: %v", err)
	}
	if !resp.Result.Success {
		t.Error("expected Result.Success = true")
	}
	if runAndPublishCalled {
		t.Error("RunAndPublish should not be called for active session")
	}
	if len(sessionRepo.updateStatusCalls) != 0 {
		t.Errorf("UpdateStatus should not be called, got %d calls", len(sessionRepo.updateStatusCalls))
	}
}

func TestOpenSession_PermissionDenied(t *testing.T) {
	t.Parallel()

	userA := uuid.New()
	userB := uuid.New()

	// Session belongs to userA.
	session := &model.Session{
		ID:         uuid.New(),
		OwnerKind:  "user",
		OwnerRefID: userA.String(),
		Status:     string(protocol.SessionStatusIdle),
	}

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == session.ID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}
	publisher := &mockPublisher{}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	// Call with userB in context.
	ctx := userCtx(t, userB)
	req := &protocol.OpenSessionRequest{SessionId: session.ID.String()}

	_, err := h.OpenSession(ctx, req)
	assertAPIError(t, err, "permission_denied")
}

func TestOpenSession_NotFound(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, _ uuid.UUID) (*model.Session, error) {
			return nil, repo.ErrSessionNotFound
		},
	}
	publisher := &mockPublisher{}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: sessionID.String()}

	_, err := h.OpenSession(ctx, req)
	assertAPIError(t, err, "session.not_found")
}

func TestOpenSession_Unauthorized(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{}
	publisher := &mockPublisher{}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	// Context with no user_id.
	ctx := context.Background()
	req := &protocol.OpenSessionRequest{SessionId: uuid.New().String()}

	_, err := h.OpenSession(ctx, req)
	assertAPIError(t, err, "unauthorized")
}

func TestOpenSession_InvalidSessionId(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionRepo := &mockSessionRepo{}
	publisher := &mockPublisher{}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)

	tests := []struct {
		name      string
		sessionID string
	}{
		{"empty string", ""},
		{"not a uuid", "not-a-uuid"},
		{"malformed uuid", "1234-5678"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := &protocol.OpenSessionRequest{SessionId: tt.sessionID}
			_, err := h.OpenSession(ctx, req)
			assertAPIError(t, err, "invalid_argument")
		})
	}
}

func TestOpenSession_InternalError_GetByID(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, _ uuid.UUID) (*model.Session, error) {
			return nil, fmt.Errorf("database connection lost")
		},
	}
	publisher := &mockPublisher{}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: sessionID.String()}

	_, err := h.OpenSession(ctx, req)
	// internalError returns an APIError with the provided code.
	assertAPIError(t, err, "session.error")
}

func TestOpenSession_InternalError_RunAndPublish(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	session := closedSession(t, userID)

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == session.ID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
		updateStatusFunc: func(_ context.Context, _ uuid.UUID, _ protocol.SessionStatus) error {
			return fmt.Errorf("db connection lost")
		},
	}

	publisher := &mockPublisher{
		runAndPublishFunc: func(_ context.Context, _ func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
			return nil, fmt.Errorf("db connection lost")
		},
	}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: session.ID.String()}

	resp, err := h.OpenSession(ctx, req)
	if resp != nil {
		t.Errorf("expected nil response, got %+v", resp)
	}
	assertAPIError(t, err, "open.error")
}

func TestOpenSession_Success_ErrPushAfterCommit(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	session := closedSession(t, userID)

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == session.ID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}

	publisher := &mockPublisher{
		runAndPublishFunc: func(_ context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
			// Execute the callback (transaction commits successfully)
			_, err := fn(context.Background())
			if err != nil {
				return nil, err
			}
			// But push phase fails
			return nil, fmt.Errorf("%w: push timeout", updates.ErrPushAfterCommit)
		},
	}
	h := newTestOpenSessionHandler(t, sessionRepo, publisher)

	ctx := userCtx(t, userID)
	req := &protocol.OpenSessionRequest{SessionId: session.ID.String()}

	resp, err := h.OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession returned unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if !resp.Result.Success {
		t.Error("expected Result.Success = true")
	}
}
