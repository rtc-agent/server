// Package rpchandler — StopTurn handler unit tests.
package rpchandler

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// Reuse testMetrics from sendmessage_test.go (same package, shared initialization)

func newTestStopTurnHandler(t *testing.T, sessionRepo *mockSessionRepo) *Handler {
	t.Helper()

	usecaseDeps := &usecase.Dependencies{
		SessionRepo: sessionRepo,
	}

	deps := &Dependencies{
		Deps:        usecaseDeps,
		SessionRepo: sessionRepo,
		Metrics:     testMetrics, // Shared with sendmessage_test.go
	}

	return &Handler{deps: deps}
}

func stopTurnCtx(t *testing.T, userID uuid.UUID) context.Context {
	t.Helper()
	return contextx.WithClientInfo(context.Background(), userID, "test-device")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestStopTurn_Unauthorized(t *testing.T) {
	t.Parallel()

	handler := newTestStopTurnHandler(t, &mockSessionRepo{})

	req := &protocol.StopTurnRequest{
		SessionId: uuid.New().String(),
	}

	// Context without user ID
	ctx := context.Background()
	_, err := handler.StopTurn(ctx, req)

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

func TestStopTurn_SessionNotFound(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, _ uuid.UUID) (*model.Session, error) {
			return nil, repo.ErrSessionNotFound
		},
	}

	handler := newTestStopTurnHandler(t, sessionRepo)

	req := &protocol.StopTurnRequest{
		SessionId: sessionID.String(),
	}

	ctx := stopTurnCtx(t, userID)
	_, err := handler.StopTurn(ctx, req)

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

func TestStopTurn_Success(t *testing.T) {
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

	handler := newTestStopTurnHandler(t, sessionRepo)

	req := &protocol.StopTurnRequest{
		SessionId: sessionID.String(),
	}

	ctx := stopTurnCtx(t, userID)
	resp, err := handler.StopTurn(ctx, req)

	if err != nil {
		t.Fatalf("StopTurn() error = %v", err)
	}
	if resp == nil {
		t.Fatal("StopTurn() response is nil")
	}
	if !resp.Result.Success {
		t.Error("StopTurn() result.success should be true")
	}
}
