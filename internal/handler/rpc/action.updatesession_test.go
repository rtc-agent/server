// Package rpchandler — UpdateSession handler unit tests.
package rpchandler

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestUpdateSessionHandler(t *testing.T, sessionRepo *mockSessionRepo, publisher *mockPublisher) *Handler {
	t.Helper()

	usecaseDeps := &usecase.Dependencies{
		SessionRepo:     sessionRepo,
		UpdatePublisher: publisher,
	}

	deps := &Dependencies{
		Deps:        usecaseDeps,
		SessionRepo: sessionRepo,
		Metrics:     testMetrics, // Shared with sendmessage_test.go
	}

	return &Handler{deps: deps}
}

func updateSessionCtx(t *testing.T, userID uuid.UUID) context.Context {
	t.Helper()
	return contextx.WithClientInfo(context.Background(), userID, "test-device")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestUpdateSession_Unauthorized(t *testing.T) {
	t.Parallel()

	handler := newTestUpdateSessionHandler(t, &mockSessionRepo{}, &mockPublisher{})

	req := &protocol.UpdateSessionRequest{
		SessionId: uuid.New().String(),
	}

	// Context without user ID
	ctx := context.Background()
	_, err := handler.UpdateSession(ctx, req)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.Code != ErrorCodeUnauthorized {
		t.Errorf("expected %s, got %s", ErrorCodeUnauthorized, apiErr.Code)
	}
}

func TestUpdateSession_SessionNotFound(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, _ uuid.UUID) (*model.Session, error) {
			return nil, repo.ErrSessionNotFound
		},
	}

	handler := newTestUpdateSessionHandler(t, sessionRepo, &mockPublisher{})

	req := &protocol.UpdateSessionRequest{
		SessionId: sessionID.String(),
	}

	ctx := updateSessionCtx(t, userID)
	_, err := handler.UpdateSession(ctx, req)

	// CheckSessionOwnership returns error when session not found
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}
	// ownershipError maps to session.not_found or session.error
	if apiErr.Code != "session.not_found" && apiErr.Code != "session.error" {
		t.Errorf("expected session error, got %s", apiErr.Code)
	}
}

func TestUpdateSession_Success(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()

	session := &model.Session{
		ID:         sessionID,
		OwnerKind:  "user",
		OwnerRefID: userID.String(),
		Status:     string(protocol.SessionStatusActive),
	}

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == sessionID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}

	publisher := &mockPublisher{
		runAndPublishFunc: func(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
			_, err := fn(ctx)
			if err != nil {
				return nil, err
			}
			return []*protocol.Update{}, nil
		},
	}

	handler := newTestUpdateSessionHandler(t, sessionRepo, publisher)

	newTitle := "Updated Title"
	req := &protocol.UpdateSessionRequest{
		SessionId: sessionID.String(),
		Title:     &newTitle,
	}

	ctx := updateSessionCtx(t, userID)
	resp, err := handler.UpdateSession(ctx, req)

	if err != nil {
		t.Fatalf("UpdateSession() error = %v", err)
	}
	if resp == nil {
		t.Fatal("UpdateSession() response is nil")
	}
	if resp.Result.SessionId != sessionID.String() {
		t.Errorf("expected session_id %s, got %s", sessionID.String(), resp.Result.SessionId)
	}
}

func TestUpdateSession_NoFields(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()

	session := &model.Session{
		ID:         sessionID,
		OwnerKind:  "user",
		OwnerRefID: userID.String(),
		Status:     string(protocol.SessionStatusActive),
	}

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == sessionID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}

	handler := newTestUpdateSessionHandler(t, sessionRepo, &mockPublisher{})

	// Request with no fields to update
	req := &protocol.UpdateSessionRequest{
		SessionId: sessionID.String(),
	}

	ctx := updateSessionCtx(t, userID)
	resp, err := handler.UpdateSession(ctx, req)

	if err != nil {
		t.Fatalf("UpdateSession() error = %v", err)
	}
	if resp == nil {
		t.Fatal("UpdateSession() response is nil")
	}
	// Should return early without calling publisher
}
