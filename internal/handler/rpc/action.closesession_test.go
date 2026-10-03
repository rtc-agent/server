// Package rpchandler — CloseSession handler unit tests.
package rpchandler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/pkg/memory"
	"github.com/rtc-agent/server/pkg/protocol"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// mockMemoryRepo implements memory.Repository for testing.
type mockMemoryRepo struct {
	deleteByScopeFunc func(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) error
}

func (m *mockMemoryRepo) Create(_ context.Context, _ *memory.Memory) error { return nil }
func (m *mockMemoryRepo) BatchCreate(_ context.Context, _ []*memory.Memory) error {
	return nil
}
func (m *mockMemoryRepo) GetByID(_ context.Context, _ uuid.UUID) (*memory.Memory, error) {
	return nil, nil
}
func (m *mockMemoryRepo) ListByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _ memory.ListOptions) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *mockMemoryRepo) ListRecentForInjection(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _, _ int) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *mockMemoryRepo) Search(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _ string, _ int) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *mockMemoryRepo) Update(_ context.Context, _ uuid.UUID, _ map[string]any, _ ...time.Time) error {
	return nil
}
func (m *mockMemoryRepo) Delete(_ context.Context, _ uuid.UUID) error { return nil }
func (m *mockMemoryRepo) DeleteByScope(ctx context.Context, scope memory.ScopeType, scopeID uuid.UUID) error {
	if m.deleteByScopeFunc != nil {
		return m.deleteByScopeFunc(ctx, scope, scopeID)
	}
	return nil
}
func (m *mockMemoryRepo) GetLinked(_ context.Context, _ uuid.UUID, _ string) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *mockMemoryRepo) CreateLink(_ context.Context, _ *memory.MemoryLink) error { return nil }
func (m *mockMemoryRepo) DeleteLink(_ context.Context, _, _ uuid.UUID) error       { return nil }
func (m *mockMemoryRepo) CountTokensByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID) (int, error) {
	return 0, nil
}

// mockUpdatePublisher implements usecase.UpdatePublisher for testing.
type mockUpdatePublisher struct {
	runAndPublishFunc func(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error)
}

func (m *mockUpdatePublisher) Publish(_ context.Context, _ ...updates.UpdatePublishItem) ([]*protocol.Update, error) {
	return nil, nil
}
func (m *mockUpdatePublisher) RunAndPublish(ctx context.Context, fn func(txCtx context.Context) ([]updates.UpdatePublishItem, error)) ([]*protocol.Update, error) {
	if m.runAndPublishFunc != nil {
		return m.runAndPublishFunc(ctx, fn)
	}
	_, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	return nil, nil
}
func (m *mockUpdatePublisher) ResolveMessageContent(_ *model.Message) string { return "" }

// mockDependencies creates a minimal Dependencies for CloseSession tests.
func mockDependencies(t *testing.T, sessionRepo *mockSessionRepo, memoryRepo *mockMemoryRepo, publisher *mockUpdatePublisher) *Dependencies {
	t.Helper()

	usecaseDeps := &usecase.Dependencies{
		SessionRepo:     sessionRepo,
		MemoryRepo:      memoryRepo,
		UpdatePublisher: publisher,
	}

	// Create a no-op metrics instance
	metrics := &turnagent.PrometheusMetrics{}

	return &Dependencies{
		Deps:        usecaseDeps,
		SessionRepo: sessionRepo,
		Metrics:     metrics,
	}
}

func TestCloseSession_NotFound(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, _ uuid.UUID) (*model.Session, error) {
			return nil, repo.ErrSessionNotFound
		},
	}

	memoryRepo := &mockMemoryRepo{}
	publisher := &mockUpdatePublisher{}
	deps := mockDependencies(t, sessionRepo, memoryRepo, publisher)
	h := &Handler{deps: deps}

	ctx := contextx.WithClientInfo(context.Background(), userID, "test-device")
	req := &protocol.CloseSessionRequest{SessionId: uuid.New().String()}

	_, err := h.CloseSession(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session.not_found")
}

func TestCloseSession_WrongOwner(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	otherUserID := uuid.New()
	sessionID := uuid.New()
	session := &model.Session{
		ID:         sessionID,
		OwnerKind:  "user",
		OwnerRefID: otherUserID.String(), // Owned by different user
		Status:     string(protocol.SessionStatusIdle),
	}

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == sessionID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}

	memoryRepo := &mockMemoryRepo{}
	publisher := &mockUpdatePublisher{}
	deps := mockDependencies(t, sessionRepo, memoryRepo, publisher)
	h := &Handler{deps: deps}

	ctx := contextx.WithClientInfo(context.Background(), userID, "test-device")
	req := &protocol.CloseSessionRequest{SessionId: sessionID.String()}

	_, err := h.CloseSession(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission_denied")
}

func TestCloseSession_AlreadyClosed(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	now := time.Now()
	sessionID := uuid.New()
	session := &model.Session{
		ID:         sessionID,
		OwnerKind:  "user",
		OwnerRefID: userID.String(),
		Status:     string(protocol.SessionStatusClosed),
		ClosedAt:   &now,
	}

	sessionRepo := &mockSessionRepo{
		getByIDFunc: func(_ context.Context, id uuid.UUID) (*model.Session, error) {
			if id == sessionID {
				return session, nil
			}
			return nil, repo.ErrSessionNotFound
		},
	}

	memoryRepo := &mockMemoryRepo{}
	publisher := &mockUpdatePublisher{}
	deps := mockDependencies(t, sessionRepo, memoryRepo, publisher)
	h := &Handler{deps: deps}

	ctx := contextx.WithClientInfo(context.Background(), userID, "test-device")
	req := &protocol.CloseSessionRequest{SessionId: sessionID.String()}

	resp, err := h.CloseSession(ctx, req)
	require.NoError(t, err)
	assert.True(t, resp.Result.Success)
}

func TestCloseSession_MissingUserID(t *testing.T) {
	t.Parallel()

	sessionRepo := &mockSessionRepo{}
	memoryRepo := &mockMemoryRepo{}
	publisher := &mockUpdatePublisher{}
	deps := mockDependencies(t, sessionRepo, memoryRepo, publisher)
	h := &Handler{deps: deps}

	// Context without user ID
	ctx := context.Background()
	req := &protocol.CloseSessionRequest{SessionId: uuid.New().String()}

	_, err := h.CloseSession(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthorized")
}
