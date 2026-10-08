// Package rpchandler — SendMessage handler unit tests.
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
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// ---------------------------------------------------------------------------
// Mock repos for SendMessage tests
// ---------------------------------------------------------------------------

type mockMessageRepo struct {
	createFunc        func(ctx context.Context, msg *model.Message) error
	getByIDFunc       func(ctx context.Context, id uuid.UUID) (*model.Message, error)
	listBySessionFunc func(ctx context.Context, sessionID uuid.UUID, beforeOffset *uint32, limit int) ([]*model.Message, error)
}

func (m *mockMessageRepo) Create(ctx context.Context, msg *model.Message) error {
	if m.createFunc != nil {
		return m.createFunc(ctx, msg)
	}
	return nil
}

func (m *mockMessageRepo) GetByID(ctx context.Context, id uuid.UUID) (*model.Message, error) {
	if m.getByIDFunc != nil {
		return m.getByIDFunc(ctx, id)
	}
	return nil, repo.ErrMessageNotFound
}

func (m *mockMessageRepo) ListBySession(ctx context.Context, sessionID uuid.UUID, beforeOffset *uint32, limit int) ([]*model.Message, error) {
	if m.listBySessionFunc != nil {
		return m.listBySessionFunc(ctx, sessionID, beforeOffset, limit)
	}
	return nil, nil
}

func (m *mockMessageRepo) GetLastOffset(_ context.Context, _ uuid.UUID) (*uint32, error) {
	return nil, nil
}

func (m *mockMessageRepo) BatchCreate(_ context.Context, _ []*model.Message) error {
	return nil
}

func (m *mockMessageRepo) FindByClientID(_ context.Context, _ string) (*model.Message, error) {
	return nil, nil
}

func (m *mockMessageRepo) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Message, error) {
	return nil, nil
}

func (m *mockMessageRepo) DeleteByIDs(_ context.Context, _ []uuid.UUID) error {
	return nil
}

func (m *mockMessageRepo) CountBySession(_ context.Context, _ uuid.UUID) (int64, error) {
	return 0, nil
}

func (m *mockMessageRepo) ListRecentBySession(_ context.Context, _ uuid.UUID, _ int) ([]*model.Message, error) {
	return nil, nil
}

func (m *mockMessageRepo) ListBySessionBeforeOffset(_ context.Context, _ uuid.UUID, _ uint32, _ int) ([]*model.Message, error) {
	return nil, nil
}

func (m *mockMessageRepo) GetNextGlobalOffset(_ context.Context, _ uuid.UUID) (uint32, error) {
	return 1, nil
}

func (m *mockMessageRepo) UpdateStreamingStatus(_ context.Context, _ uuid.UUID, _ protocol.MessageStreamingStatus, _ string) error {
	return nil
}

func (m *mockMessageRepo) UpdateTokenUsage(_ context.Context, _ uuid.UUID, _ *model.TokenUsageUpdate) error {
	return nil
}

func (m *mockMessageRepo) ListForAdmin(_ context.Context, _ uuid.UUID, _ repo.MessageAdminFilter) ([]*model.Message, int64, error) {
	return nil, 0, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testMetrics is a shared metrics instance for all tests to avoid duplicate registration
var testMetrics = turnagent.NewPrometheusMetrics()

func newTestSendMessageHandler(t *testing.T, sessionRepo *mockSessionRepo, messageRepo *mockMessageRepo, publisher *mockPublisher) *Handler {
	t.Helper()

	usecaseDeps := &usecase.Dependencies{
		SessionRepo:     sessionRepo,
		MessageRepo:     messageRepo,
		UpdatePublisher: publisher,
		// Redis is nil here - only error path tests that fail before Redis
		// is accessed can use this helper.
	}

	deps := &Dependencies{
		Deps:        usecaseDeps,
		SessionRepo: sessionRepo,
		Metrics:     testMetrics,
	}

	return &Handler{deps: deps}
}

func sendMsgCtx(t *testing.T, userID uuid.UUID, _ string) context.Context {
	t.Helper()
	return contextx.WithClientInfo(context.Background(), userID, "test-device")
}

// ---------------------------------------------------------------------------
// Tests — Error Paths (no Redis required)
// ---------------------------------------------------------------------------

func TestSendMessage_Unauthorized(t *testing.T) {
	t.Parallel()

	handler := newTestSendMessageHandler(t, &mockSessionRepo{}, &mockMessageRepo{}, &mockPublisher{})

	req := &protocol.SendMessageRequest{
		ClientId:        uuid.New().String(),
		ClientSessionId: uuid.New().String(),
		ContentData: protocol.ContentData{
			Type: protocol.ContentTypeText,
			Data: []byte(`{"text":"test"}`),
		},
	}

	// Context without user ID
	ctx := context.Background()
	_, err := handler.SendMessage(ctx, req)

	assertAPIError(t, err, ErrorCodeUnauthorized)
}

func TestSendMessage_SessionNotFound(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionID := uuid.New()
	serverSessionID := sessionID.String()

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, _ uuid.UUID) (*model.Session, error) {
			return nil, repo.ErrSessionNotFound
		},
	}

	handler := newTestSendMessageHandler(t, sessionRepo, &mockMessageRepo{}, &mockPublisher{})

	// Use ServerSessionId to trigger session lookup (not creation)
	req := &protocol.SendMessageRequest{
		ClientId:        uuid.New().String(),
		ClientSessionId: "test-client-session",
		ServerSessionId: &serverSessionID, // This triggers session lookup
		ContentData: protocol.ContentData{
			Type: protocol.ContentTypeText,
			Data: []byte(`{"text":"test"}`),
		},
	}

	ctx := sendMsgCtx(t, userID, sessionID.String())
	_, err := handler.SendMessage(ctx, req)

	// When session not found, PrepareSession returns error which becomes "session.error" internal error
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.Code != "session.error" {
		t.Errorf("expected session.error, got %s", apiErr.Code)
	}
}

// TestSendMessage_PermissionDenied is removed because:
// - PrepareSession checks ownership and returns an error if mismatched
// - The handler wraps this as an internal error (not PermissionDenied)
// - But the test would still need Redis to complete the flow
//
// Ownership validation is covered by primitives_test.go if needed.
