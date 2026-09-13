package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/pkg/protocol"
)

// mockSessionRepo is a minimal in-memory mock for SessionRepo,
// implementing only the methods used by TokenEstimator tests.
type mockSessionRepo struct {
	sessions map[uuid.UUID]*model.Session
}

func newMockSessionRepo() *mockSessionRepo {
	return &mockSessionRepo{
		sessions: make(map[uuid.UUID]*model.Session),
	}
}

func (m *mockSessionRepo) addSession(s *model.Session) {
	m.sessions[s.ID] = s
}

func (m *mockSessionRepo) GetByID(_ context.Context, id uuid.UUID) (*model.Session, error) {
	if s, ok := m.sessions[id]; ok {
		return s, nil
	}
	return nil, repo.ErrSessionNotFound
}

func (m *mockSessionRepo) AtomicUpdateEWMA(_ context.Context, sessionID uuid.UUID, ewma float64) error {
	if s, ok := m.sessions[sessionID]; ok {
		s.TokenEstimateEWMA = ewma
	}
	return nil
}

// Stubs for unused interface methods:

func (m *mockSessionRepo) Create(_ context.Context, _ *model.Session) error {
	panic("not implemented")
}
func (m *mockSessionRepo) FindByClientID(_ context.Context, _ string) (*model.Session, error) {
	panic("not implemented")
}
func (m *mockSessionRepo) GetByUser(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Session, error) {
	panic("not implemented")
}
func (m *mockSessionRepo) UpdateStatus(_ context.Context, _ uuid.UUID, _ protocol.SessionStatus) error {
	panic("not implemented")
}
func (m *mockSessionRepo) Update(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	panic("not implemented")
}
func (m *mockSessionRepo) TouchActive(_ context.Context, _ uuid.UUID) error {
	panic("not implemented")
}
func (m *mockSessionRepo) UpdateFieldsActive(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	panic("not implemented")
}
func (m *mockSessionRepo) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Session, error) {
	panic("not implemented")
}
func (m *mockSessionRepo) ListByRoot(_ context.Context, _ uuid.UUID, _ string) ([]*model.Session, error) {
	panic("not implemented")
}
func (m *mockSessionRepo) AtomicAddTokenUsage(_ context.Context, _ uuid.UUID, _ repo.TokenUsageDelta) error {
	panic("not implemented")
}

// newTestEstimator creates a TokenEstimator backed by a mock SessionRepo.
func newTestEstimator(t *testing.T) (*TokenEstimator, *mockSessionRepo) {
	t.Helper()
	mockRepo := newMockSessionRepo()
	est := NewTokenEstimator(25000, 13000, mockRepo)
	return est, mockRepo
}

// newSession creates a session in the mock repo with the given TotalTokens and EWMA.
func newSession(repo *mockSessionRepo, totalTokens int64, ewma float64) uuid.UUID {
	sid := uuid.New()
	repo.addSession(&model.Session{
		ID:                sid,
		TotalTokens:       totalTokens,
		TokenEstimateEWMA: ewma,
	})
	return sid
}

func TestTokenEstimator_Estimate_FirstCall(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()
	// Session with default EWMA = 0 (first call, will fall back to defaultGrowth)
	sid := newSession(mockRepo, 5000, 0)

	// First call: prevEWMA=0, should use defaultGrowth (2000)
	result := est.Estimate(ctx, sid, 5000, 0, 3000)

	if result.CurrentTokens != 5000 {
		t.Errorf("CurrentTokens = %d, want 5000", result.CurrentTokens)
	}
	if result.CompressionThreshold != 12000 { // 25000 - 13000
		t.Errorf("CompressionThreshold = %d, want 12000", result.CompressionThreshold)
	}
	// ewma = 0.3 * 2000 + 0.7 * 3000 = 600 + 2100 = 2700
	// EstimatedNextRound = 5000 + 2700 = 7700
	if result.EstimatedNextRound != 7700 {
		t.Errorf("EstimatedNextRound = %d, want 7700", result.EstimatedNextRound)
	}
	// progress = 5000/12000 * 100 = 41.666...
	if result.CompressionProgress < 41.0 || result.CompressionProgress > 42.0 {
		t.Errorf("CompressionProgress = %f, want ~41.67", result.CompressionProgress)
	}
	// roundsUntil = (12000 - 5000) / 2700 = 2.59 → 2
	if result.RoundsUntilCompression != 2 {
		t.Errorf("RoundsUntilCompression = %d, want 2", result.RoundsUntilCompression)
	}
	// NewEWMA should be 2700
	if result.NewEWMA != 2700 {
		t.Errorf("NewEWMA = %f, want 2700", result.NewEWMA)
	}
}

func TestTokenEstimator_Estimate_WithPrevEWMA(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()
	// Session with prev EWMA = 2700 (from a previous call)
	sid := newSession(mockRepo, 8000, 2700)

	// Second call: should use prevEWMA (2700)
	result := est.Estimate(ctx, sid, 8000, 2700, 4000)

	if result.CurrentTokens != 8000 {
		t.Errorf("CurrentTokens = %d, want 8000", result.CurrentTokens)
	}
	// ewma = 0.3 * 2700 + 0.7 * 4000 = 810 + 2800 = 3610
	// EstimatedNextRound = 8000 + 3610 = 11610
	if result.EstimatedNextRound != 11610 {
		t.Errorf("EstimatedNextRound = %d, want 11610", result.EstimatedNextRound)
	}
	if result.NewEWMA != 3610 {
		t.Errorf("NewEWMA = %f, want 3610", result.NewEWMA)
	}
}

func TestTokenEstimator_Estimate_ExceedsThreshold(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()
	// Current tokens already exceeds threshold (12000)
	sid := newSession(mockRepo, 15000, 2000)

	result := est.Estimate(ctx, sid, 15000, 2000, 3000)

	if result.RoundsUntilCompression != -1 {
		t.Errorf("RoundsUntilCompression = %d, want -1", result.RoundsUntilCompression)
	}
	if result.CompressionProgress != 100 {
		t.Errorf("CompressionProgress = %f, want 100", result.CompressionProgress)
	}
}

func TestTokenEstimator_ReadEWMA(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()

	// Session with EWMA = 2700
	sid := newSession(mockRepo, 5000, 2700)

	ewma := est.ReadEWMA(ctx, sid)
	if ewma != 2700 {
		t.Errorf("ReadEWMA = %f, want 2700", ewma)
	}

	// Non-existent session should return 0
	unknownID := uuid.New()
	ewma = est.ReadEWMA(ctx, unknownID)
	if ewma != 0 {
		t.Errorf("ReadEWMA for unknown session = %f, want 0", ewma)
	}
}

func TestNewTokenEstimator_Defaults(t *testing.T) {
	mockRepo := newMockSessionRepo()

	// Test threshold calculation
	est := NewTokenEstimator(25000, 13000, mockRepo)
	if est.triggerThreshold != 12000 {
		t.Errorf("triggerThreshold = %d, want 12000", est.triggerThreshold)
	}

	// Test negative threshold protection
	est2 := NewTokenEstimator(100, 200, mockRepo)
	if est2.triggerThreshold != 1 {
		t.Errorf("negative threshold should be clamped to 1, got %d", est2.triggerThreshold)
	}
}

func TestTokenEstimator_ReestimateAfterCompact_NormalCompression(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()

	// Session with TotalTokens=6000 (post-compact), prevEWMA=2700
	sid := newSession(mockRepo, 6000, 2700)

	// Simulate compression: 10000 → 6000 tokens (ratio = 0.6)
	// Expected new EWMA = 2700 * 0.6 = 1620
	// CurrentTokens from session = 6000
	// EstimatedNextRound = 6000 + 1620 = 7620
	result, err := est.ReestimateAfterCompact(ctx, sid, 2700, 10000, 6000)
	if err != nil {
		t.Fatalf("ReestimateAfterCompact: %v", err)
	}

	if result.CurrentTokens != 6000 {
		t.Errorf("CurrentTokens = %d, want 6000", result.CurrentTokens)
	}
	// ewma = 2700 * 0.6 = 1620
	// EstimatedNextRound = 6000 + 1620 = 7620
	if result.EstimatedNextRound != 7620 {
		t.Errorf("EstimatedNextRound = %d, want 7620", result.EstimatedNextRound)
	}
	// progress = 6000/12000 * 100 = 50
	if result.CompressionProgress != 50.0 {
		t.Errorf("CompressionProgress = %f, want 50.0", result.CompressionProgress)
	}
	// roundsUntil = (12000 - 6000) / 1620 = 3.7 → 3
	if result.RoundsUntilCompression != 3 {
		t.Errorf("RoundsUntilCompression = %d, want 3", result.RoundsUntilCompression)
	}

	// Verify EWMA was persisted to the session.
	if mockRepo.sessions[sid].TokenEstimateEWMA != 1620 {
		t.Errorf("persisted EWMA = %f, want 1620", mockRepo.sessions[sid].TokenEstimateEWMA)
	}
}

func TestTokenEstimator_ReestimateAfterCompact_NoPreviousEWMA(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()

	// Session with no previous EWMA (0), TotalTokens=5000
	sid := newSession(mockRepo, 5000, 0)

	// No previous EWMA → falls back to defaultGrowth (2000)
	result, err := est.ReestimateAfterCompact(ctx, sid, 0, 10000, 5000)
	if err != nil {
		t.Fatalf("ReestimateAfterCompact: %v", err)
	}

	if result.CurrentTokens != 5000 {
		t.Errorf("CurrentTokens = %d, want 5000", result.CurrentTokens)
	}
	// ewma = defaultGrowth = 2000 (no previous EWMA)
	// EstimatedNextRound = 5000 + 2000 = 7000
	if result.EstimatedNextRound != 7000 {
		t.Errorf("EstimatedNextRound = %d, want 7000", result.EstimatedNextRound)
	}
}

func TestTokenEstimator_ReestimateAfterCompact_SummaryLongerThanOriginal(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()

	// Session with TotalTokens=11000 (post-compact), prevEWMA=2700
	sid := newSession(mockRepo, 11000, 2700)

	// Edge case: summary longer than original → ratio capped at 1.0
	result, err := est.ReestimateAfterCompact(ctx, sid, 2700, 10000, 11000)
	if err != nil {
		t.Fatalf("ReestimateAfterCompact: %v", err)
	}

	// ewma should stay at 2700 (ratio capped at 1.0)
	// CurrentTokens = 11000 (from session)
	// EstimatedNextRound = 11000 + 2700 = 13700
	if result.EstimatedNextRound != 13700 {
		t.Errorf("EstimatedNextRound = %d, want 13700", result.EstimatedNextRound)
	}
	if mockRepo.sessions[sid].TokenEstimateEWMA != 2700 {
		t.Errorf("persisted EWMA = %f, want 2700", mockRepo.sessions[sid].TokenEstimateEWMA)
	}
}

func TestTokenEstimator_ReestimateAfterCompact_ConsecutiveCompacts(t *testing.T) {
	est, mockRepo := newTestEstimator(t)
	ctx := context.Background()

	// Initial session: TotalTokens=15000, prevEWMA=3400
	sid := newSession(mockRepo, 15000, 3400)

	// First compact: 15000 → 8000 (ratio = 8000/15000 = 0.533)
	// ewma = 3400 * 0.533 = 1813.3
	// Update session TotalTokens to post-compact value before calling ReestimateAfterCompact
	// (matching the real flow where TotalTokens is updated during compressContext).
	mockRepo.sessions[sid].TotalTokens = 8000
	result1, err := est.ReestimateAfterCompact(ctx, sid, 3400, 15000, 8000)
	if err != nil {
		t.Fatalf("first ReestimateAfterCompact: %v", err)
	}

	// After first compact: save the new EWMA, update session TotalTokens to post-compact value.
	ewma1 := mockRepo.sessions[sid].TokenEstimateEWMA
	mockRepo.sessions[sid].TotalTokens = 5000

	// Second compact: 8000 → 5000 (ratio = 5000/8000 = 0.625)
	// ewma = 1813.3 * 0.625 = 1133.3
	result2, err := est.ReestimateAfterCompact(ctx, sid, ewma1, 8000, 5000)
	if err != nil {
		t.Fatalf("second ReestimateAfterCompact: %v", err)
	}

	// After two consecutive compacts, EWMA should have decreased significantly.
	if result2.CurrentTokens != 5000 {
		t.Errorf("CurrentTokens = %d, want 5000", result2.CurrentTokens)
	}
	if result2.RoundsUntilCompression <= result1.RoundsUntilCompression {
		// More compression → more rounds until next compression
		t.Errorf("expected roundsUntil to increase after second compact, got %d <= %d",
			result2.RoundsUntilCompression, result1.RoundsUntilCompression)
	}
}
