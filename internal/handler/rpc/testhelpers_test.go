// Package rpchandler — shared test helpers for RPC handler tests.
package rpchandler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/memory"
	"github.com/rtc-agent/server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// No-op RtcRepo for tests
// ---------------------------------------------------------------------------

type noopRtcRepo struct{}

func (m *noopRtcRepo) Create(_ context.Context, _ *model.Rtc) error { return nil }
func (m *noopRtcRepo) GetByID(_ context.Context, _ uuid.UUID) (*model.Rtc, error) {
	return nil, nil
}
func (m *noopRtcRepo) FindByClientID(_ context.Context, _ string) (*model.Rtc, error) {
	return nil, nil
}
func (m *noopRtcRepo) ListBySession(_ context.Context, _ uuid.UUID, _ *string, _ int) ([]*model.Rtc, error) {
	return nil, nil
}
func (m *noopRtcRepo) ListByTurn(_ context.Context, _ uuid.UUID) ([]*model.Rtc, error) {
	return nil, nil
}
func (m *noopRtcRepo) ListByMessageIDs(_ context.Context, _ []uuid.UUID) ([]*model.Rtc, error) {
	return nil, nil
}
func (m *noopRtcRepo) UpdateStatus(_ context.Context, _ uuid.UUID, _ protocol.RtcStatus) error {
	return nil
}
func (m *noopRtcRepo) UpdateResult(_ context.Context, _ uuid.UUID, _ protocol.RtcStatus, _ *string, _ string) error {
	return nil
}
func (m *noopRtcRepo) UpdateOutputMessageID(_ context.Context, _, _ uuid.UUID) error { return nil }
func (m *noopRtcRepo) GetByIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]*model.Rtc, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// No-op MemoryRepo for tests
// ---------------------------------------------------------------------------

type noopMemoryRepo struct{}

func (m *noopMemoryRepo) Create(_ context.Context, _ *memory.Memory) error { return nil }
func (m *noopMemoryRepo) BatchCreate(_ context.Context, _ []*memory.Memory) error {
	return nil
}
func (m *noopMemoryRepo) GetByID(_ context.Context, _ uuid.UUID) (*memory.Memory, error) {
	return nil, nil
}
func (m *noopMemoryRepo) ListByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _ memory.ListOptions) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *noopMemoryRepo) ListRecentForInjection(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _, _ int) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *noopMemoryRepo) Search(_ context.Context, _ memory.ScopeType, _ uuid.UUID, _ string, _ int) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *noopMemoryRepo) Update(_ context.Context, _ uuid.UUID, _ map[string]any, _ ...time.Time) error {
	return nil
}
func (m *noopMemoryRepo) Delete(_ context.Context, _ uuid.UUID) error { return nil }
func (m *noopMemoryRepo) DeleteByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID) error {
	return nil
}
func (m *noopMemoryRepo) GetLinked(_ context.Context, _ uuid.UUID, _ string) ([]*memory.Memory, error) {
	return nil, nil
}
func (m *noopMemoryRepo) CreateLink(_ context.Context, _ *memory.MemoryLink) error { return nil }
func (m *noopMemoryRepo) DeleteLink(_ context.Context, _, _ uuid.UUID) error       { return nil }
func (m *noopMemoryRepo) CountTokensByScope(_ context.Context, _ memory.ScopeType, _ uuid.UUID) (int, error) {
	return 0, nil
}

// ---------------------------------------------------------------------------
// No-op ScriptExecutionRepo for tests
// ---------------------------------------------------------------------------

type noopScriptExecutionRepo struct{}

func (m *noopScriptExecutionRepo) Create(_ context.Context, _ *model.ScriptExecution) error {
	return nil
}
func (m *noopScriptExecutionRepo) GetByRtcID(_ context.Context, _ uuid.UUID) (*model.ScriptExecution, error) {
	return nil, nil
}
func (m *noopScriptExecutionRepo) ListBySession(_ context.Context, _ uuid.UUID, _, _ int) ([]*model.ScriptExecution, int64, error) {
	return nil, 0, nil
}
func (m *noopScriptExecutionRepo) ListByUser(_ context.Context, _ uuid.UUID, _, _ int) ([]*model.ScriptExecution, int64, error) {
	return nil, 0, nil
}
