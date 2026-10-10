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

	// Create minimal mock repos for RtcSessionUsecase
	messageRepo := &noopMessageRepoForUpdate{}
	turnRepo := &noopTurnRepoForUpdate{}

	usecaseDeps := &usecase.Dependencies{
		SessionRepo:       sessionRepo,
		MessageRepo:       messageRepo,
		TurnRepo:          turnRepo,
		UpdatePublisher:   publisher,
		RtcSessionUsecase: usecase.NewRtcSessionUsecase(sessionRepo, messageRepo, turnRepo),
	}

	deps := &Dependencies{
		Deps:    usecaseDeps,
		Metrics: testMetrics, // Shared with sendmessage_test.go
	}

	return &Handler{deps: deps}
}

// noopMessageRepoForUpdate is a minimal no-op implementation of repo.MessageRepo for tests.
type noopMessageRepoForUpdate struct{}

func (m *noopMessageRepoForUpdate) Create(_ context.Context, _ *model.Message) error { return nil }
func (m *noopMessageRepoForUpdate) BatchCreate(_ context.Context, _ []*model.Message) error {
	return nil
}
func (m *noopMessageRepoForUpdate) GetByID(_ context.Context, _ uuid.UUID) (*model.Message, error) {
	return nil, repo.ErrMessageNotFound
}
func (m *noopMessageRepoForUpdate) FindByClientID(_ context.Context, _ string) (*model.Message, error) {
	return nil, nil
}
func (m *noopMessageRepoForUpdate) ListBySession(_ context.Context, _ uuid.UUID, _ *uint32, _ int) ([]*model.Message, error) {
	return nil, nil
}
func (m *noopMessageRepoForUpdate) ListRecentBySession(_ context.Context, _ uuid.UUID, _ int) ([]*model.Message, error) {
	return nil, nil
}
func (m *noopMessageRepoForUpdate) ListBySessionBeforeOffset(_ context.Context, _ uuid.UUID, _ uint32, _ uint32, _ int) ([]*model.Message, error) {
	return nil, nil
}
func (m *noopMessageRepoForUpdate) GetNextGlobalOffset(_ context.Context, _ uuid.UUID) (uint32, error) {
	return 1, nil
}
func (m *noopMessageRepoForUpdate) UpdateStreamingStatus(_ context.Context, _ uuid.UUID, _ protocol.MessageStreamingStatus, _ string) error {
	return nil
}
func (m *noopMessageRepoForUpdate) DeleteByIDs(_ context.Context, _ []uuid.UUID) error { return nil }
func (m *noopMessageRepoForUpdate) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Message, error) {
	return nil, nil
}
func (m *noopMessageRepoForUpdate) UpdateTokenUsage(_ context.Context, _ uuid.UUID, _ *model.TokenUsageUpdate) error {
	return nil
}
func (m *noopMessageRepoForUpdate) ListForAdmin(_ context.Context, _ uuid.UUID, _ repo.MessageAdminFilter) ([]*model.Message, int64, error) {
	return nil, 0, nil
}

// noopTurnRepoForUpdate is a minimal no-op implementation of repo.TurnRepo for tests.
type noopTurnRepoForUpdate struct{}

func (m *noopTurnRepoForUpdate) Create(_ context.Context, _ *model.Turn) error { return nil }
func (m *noopTurnRepoForUpdate) GetByID(_ context.Context, _ uuid.UUID) (*model.Turn, error) {
	return nil, repo.ErrTurnNotFound
}
func (m *noopTurnRepoForUpdate) FindByClientID(_ context.Context, _ string) (*model.Turn, error) {
	return nil, nil
}
func (m *noopTurnRepoForUpdate) ListBySession(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Turn, error) {
	return nil, nil
}
func (m *noopTurnRepoForUpdate) FindActiveBySession(_ context.Context, _ uuid.UUID) ([]*model.Turn, error) {
	return nil, nil
}
func (m *noopTurnRepoForUpdate) FindStaleTurns(_ context.Context, _ []string) ([]*model.Turn, error) {
	return nil, nil
}
func (m *noopTurnRepoForUpdate) UpdateStatus(_ context.Context, _ uuid.UUID, _ protocol.TurnStatus, _ string) error {
	return nil
}
func (m *noopTurnRepoForUpdate) UpdateStatusBySession(_ context.Context, _ uuid.UUID, _ []string, _ protocol.TurnStatus) (int64, error) {
	return 0, nil
}
func (m *noopTurnRepoForUpdate) UpdateStatusAndInterruptID(_ context.Context, _ uuid.UUID, _ protocol.TurnStatus, _ string) error {
	return nil
}
func (m *noopTurnRepoForUpdate) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Turn, error) {
	return nil, nil
}
func (m *noopTurnRepoForUpdate) CountBySessionAndStatus(_ context.Context, _ uuid.UUID, _ string) (int64, error) {
	return 0, nil
}
func (m *noopTurnRepoForUpdate) FindStaleTurnsWithLimit(_ context.Context, _ []string, _ int) ([]*model.Turn, error) {
	return nil, nil
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
