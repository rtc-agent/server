package updates

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/protocol"
)

// TestConvertUpdates_BatchBehavior verifies that convertUpdates calls each
// entity resolver only once regardless of how many updates are provided.
// This prevents N+1 query issues where each update would trigger separate
// resolver calls.
func TestConvertUpdates_BatchBehavior(t *testing.T) {
	// Track resolver call counts
	var sessionCallCount atomic.Int32
	var messageCallCount atomic.Int32

	// Create test entity IDs
	sessionID1 := uuid.New()
	sessionID2 := uuid.New()
	messageID1 := uuid.New()
	messageID2 := uuid.New()

	// Create UpdatePublisher with counting resolvers
	u := &UpdatePublisher{
		resolvers: map[string]EntityResolver{
			string(protocol.EntitySession): func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
				sessionCallCount.Add(1)
				result := make(map[uuid.UUID]any, len(ids))
				for _, id := range ids {
					result[id] = protocol.Session{Id: id.String()}
				}
				return result, nil
			},
			string(protocol.EntityMessage): func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
				messageCallCount.Add(1)
				result := make(map[uuid.UUID]any, len(ids))
				for _, id := range ids {
					result[id] = protocol.Message{Id: id.String()}
				}
				return result, nil
			},
		},
	}

	// Create multiple updates with mixed entity types
	updates := []*model.UserUpdate{
		{
			Offset: 1,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID1.String()},
			},
		},
		{
			Offset: 2,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntityMessage, EntityId: messageID1.String()},
			},
		},
		{
			Offset: 3,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID2.String()},
				protocol.UpdateItem{Entity: protocol.EntityMessage, EntityId: messageID2.String()},
			},
		},
	}

	// Call convertUpdates
	result, err := u.convertUpdates(context.Background(), updates)
	if err != nil {
		t.Fatalf("convertUpdates failed: %v", err)
	}

	// Verify results
	if len(result) != 3 {
		t.Errorf("expected 3 updates, got %d", len(result))
	}

	// Critical: verify batch behavior - each resolver should be called exactly once
	// NOT 3 times (once per update) or 2 times (once per session/message entity)
	if count := sessionCallCount.Load(); count != 1 {
		t.Errorf("session resolver called %d times, want 1 (batch)", count)
	}
	if count := messageCallCount.Load(); count != 1 {
		t.Errorf("message resolver called %d times, want 1 (batch)", count)
	}
}

// TestConvertUpdates_BatchDeduplicatesIDs verifies that when multiple updates
// reference the same entity ID, the resolver is called with deduplicated IDs.
func TestConvertUpdates_BatchDeduplicatesIDs(t *testing.T) {
	var capturedIDs []uuid.UUID

	sessionID := uuid.New()

	u := &UpdatePublisher{
		resolvers: map[string]EntityResolver{
			string(protocol.EntitySession): func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
				capturedIDs = ids
				result := make(map[uuid.UUID]any, len(ids))
				for _, id := range ids {
					result[id] = protocol.Session{Id: id.String()}
				}
				return result, nil
			},
		},
	}

	// Multiple updates referencing the same session ID
	updates := []*model.UserUpdate{
		{
			Offset: 1,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID.String()},
			},
		},
		{
			Offset: 2,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID.String()},
			},
		},
		{
			Offset: 3,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID.String()},
			},
		},
	}

	result, err := u.convertUpdates(context.Background(), updates)
	if err != nil {
		t.Fatalf("convertUpdates failed: %v", err)
	}

	if len(result) != 3 {
		t.Errorf("expected 3 updates, got %d", len(result))
	}

	// Verify deduplication: resolver should receive only 1 unique ID, not 3
	if len(capturedIDs) != 1 {
		t.Errorf("resolver received %d IDs, want 1 (deduplicated)", len(capturedIDs))
	}
	if len(capturedIDs) > 0 && capturedIDs[0] != sessionID {
		t.Errorf("resolver received wrong ID: got %v, want %v", capturedIDs[0], sessionID)
	}
}

// TestConvertUpdates_EmptyUpdates verifies that convertUpdates handles empty
// input gracefully.
func TestConvertUpdates_EmptyUpdates(t *testing.T) {
	u := &UpdatePublisher{
		resolvers: map[string]EntityResolver{},
	}

	result, err := u.convertUpdates(context.Background(), nil)
	if err != nil {
		t.Fatalf("convertUpdates(nil) failed: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 updates, got %d", len(result))
	}

	result, err = u.convertUpdates(context.Background(), []*model.UserUpdate{})
	if err != nil {
		t.Fatalf("convertUpdates([]) failed: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 updates, got %d", len(result))
	}
}

// TestConvertUpdates_PreservesOrderAndOffset verifies that convertUpdates
// maintains the original order and offset values of updates.
func TestConvertUpdates_PreservesOrderAndOffset(t *testing.T) {
	sessionID1 := uuid.New()
	sessionID2 := uuid.New()
	sessionID3 := uuid.New()

	u := &UpdatePublisher{
		resolvers: map[string]EntityResolver{
			string(protocol.EntitySession): func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]any, error) {
				result := make(map[uuid.UUID]any, len(ids))
				for _, id := range ids {
					result[id] = protocol.Session{Id: id.String()}
				}
				return result, nil
			},
		},
	}

	updates := []*model.UserUpdate{
		{
			Offset: 100,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID1.String()},
			},
		},
		{
			Offset: 200,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID2.String()},
			},
		},
		{
			Offset: 300,
			Items: model.UpdateItemArray{
				protocol.UpdateItem{Entity: protocol.EntitySession, EntityId: sessionID3.String()},
			},
		},
	}

	result, err := u.convertUpdates(context.Background(), updates)
	if err != nil {
		t.Fatalf("convertUpdates failed: %v", err)
	}

	// Verify order and offsets are preserved
	expectedOffsets := []uint32{100, 200, 300}
	for i, update := range result {
		if update.Offset != expectedOffsets[i] {
			t.Errorf("result[%d].Offset = %d, want %d", i, update.Offset, expectedOffsets[i])
		}
		if len(update.Items) != 1 {
			t.Errorf("result[%d] has %d items, want 1", i, len(update.Items))
			continue
		}
		// Verify the entity data is resolved
		if update.DataList == nil || len(*update.DataList) != 1 {
			t.Errorf("result[%d] DataList missing or wrong length", i)
			continue
		}
		data := (*update.DataList)[0]
		if data == nil {
			t.Errorf("result[%d] DataList[0] is nil", i)
		}
	}
}
