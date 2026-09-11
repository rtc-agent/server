package turnagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// newTestQueue spins up a miniredis server and returns a Queue bound to it.
func newTestQueue(t *testing.T) (*rtcqueue.Queue, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	return rtcqueue.New(rdb), mr
}

// mockAgent is a minimal adk.Agent for testing.
type mockAgent struct {
	name    string
	runFunc func(ctx context.Context, input *adk.AgentInput) (*adk.AgentOutput, error)
}

func (a *mockAgent) Name(_ context.Context) string        { return a.name }
func (a *mockAgent) Description(_ context.Context) string { return "mock agent" }

func (a *mockAgent) Run(ctx context.Context, input *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		if a.runFunc != nil {
			out, err := a.runFunc(ctx, input)
			if err != nil {
				gen.Send(&adk.AgentEvent{Err: err})
				return
			}
			gen.Send(&adk.AgentEvent{Output: out})
			return
		}
		gen.Send(&adk.AgentEvent{Output: &adk.AgentOutput{}})
	}()
	return iter
}

// publishWorkJSON marshals a WorkPayload and publishes it to the queue.
func publishWorkJSON(t *testing.T, ctx context.Context, q *rtcqueue.Queue, sessionID string, payload WorkPayload, priority int64) string {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	id, err := q.Publish(ctx, sessionID, string(data), priority)
	require.NoError(t, err)
	return id
}

// noopLogFn is a no-op log function for tests.
func noopLogFn(_ context.Context, _ LogLevel, _ string, _ map[string]any) {}

// ---------------------------------------------------------------------------
// 1. WorkTracker Tests
// ---------------------------------------------------------------------------

func TestWorkTracker_RegisterAndComplete(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	ch := tracker.Register("work-1")
	require.NotNil(t, ch)

	// Channel should be open.
	select {
	case <-ch:
		t.Fatal("channel should not be closed yet")
	default:
	}

	// Complete should close the channel.
	tracker.Complete("work-1")

	select {
	case <-ch:
		// OK
	case <-time.After(time.Second):
		t.Fatal("channel should be closed after Complete")
	}
}

func TestWorkTracker_CompleteUnknown(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	// Completing an unknown workID should not panic.
	assert.NotPanics(t, func() {
		tracker.Complete("unknown")
	})
}

func TestWorkTracker_CompleteAll(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	ch1 := tracker.Register("work-1")
	ch2 := tracker.Register("work-2")
	ch3 := tracker.Register("work-3")

	tracker.CompleteAll()

	for i, ch := range []<-chan struct{}{ch1, ch2, ch3} {
		select {
		case <-ch:
			// OK
		case <-time.After(time.Second):
			t.Fatalf("channel %d should be closed after CompleteAll", i+1)
		}
	}

	// Calling CompleteAll again should not panic.
	assert.NotPanics(t, func() {
		tracker.CompleteAll()
	})
}

func TestWorkTracker_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	var wg sync.WaitGroup
	// 100 goroutines register.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			workID := "work-" + string(rune('A'+id%26)) + "-" + string(rune('0'+id/26))
			tracker.Register(workID)
		}(i)
	}

	// 50 goroutines complete.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			workID := "work-" + string(rune('A'+id%26)) + "-" + string(rune('0'+id/26))
			tracker.Complete(workID)
		}(i)
	}

	wg.Wait()

	// CompleteAll to clean up.
	tracker.CompleteAll()
}

func TestWorkTracker_IdempotentComplete(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	ch := tracker.Register("work-1")

	// Multiple Complete calls should not panic.
	tracker.Complete("work-1")
	assert.NotPanics(t, func() {
		tracker.Complete("work-1")
	})

	// Channel should still be closed.
	select {
	case <-ch:
	default:
		t.Fatal("channel should be closed")
	}
}

// ---------------------------------------------------------------------------
// 2. SessionManagerRegistry Tests
// ---------------------------------------------------------------------------

func TestRegistry_GetOrCreate_New(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-1"

	// Publish work so claim can succeed.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	registry := NewSessionManagerRegistry()
	cfg := minimalConfig(t)

	mgr, isNew, err := registry.GetOrCreate(ctx, q, sessionID, "worker-1", "turn-1", "cp-1", "", cfg, noopLogFn)
	require.NoError(t, err)
	require.NotNil(t, mgr)
	assert.True(t, isNew)
	assert.Equal(t, sessionID, mgr.SessionID())

	// Wait for loop to idle-exit, then wait for full cleanup.
	<-mgr.Done()
}

func TestRegistry_GetOrCreate_Existing(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-2"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	registry := NewSessionManagerRegistry()
	cfg := minimalConfig(t)

	mgr1, isNew1, err := registry.GetOrCreate(ctx, q, sessionID, "worker-1", "turn-1", "cp-1", "", cfg, noopLogFn)
	require.NoError(t, err)
	require.True(t, isNew1)

	// Second GetOrCreate should return the same manager.
	mgr2, isNew2, err := registry.GetOrCreate(ctx, q, sessionID, "worker-1", "turn-2", "cp-1", "", cfg, noopLogFn)
	require.NoError(t, err)
	require.False(t, isNew2)
	assert.Equal(t, mgr1, mgr2)

	// Wait for cleanup.
	<-mgr1.Done()
}

func TestRegistry_Remove(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-3"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	registry := NewSessionManagerRegistry()
	cfg := minimalConfig(t)

	mgr, _, err := registry.GetOrCreate(ctx, q, sessionID, "worker-1", "turn-1", "cp-1", "", cfg, noopLogFn)
	require.NoError(t, err)

	assert.NotNil(t, registry.Get(sessionID))

	registry.Remove(sessionID)
	assert.Nil(t, registry.Get(sessionID))

	// Wait for cleanup.
	<-mgr.Done()
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()

	registry := NewSessionManagerRegistry()
	cfg := minimalConfig(t)

	var wg sync.WaitGroup
	var managers sync.Map

	// Create managers for 20 different sessions concurrently.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sessionID := fmt.Sprintf("session-concurrent-%d", id)
			publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)
			mgr, _, err := registry.GetOrCreate(ctx, q, sessionID, "worker-1", "turn-1", "cp-1", "", cfg, noopLogFn)
			if err != nil {
				return
			}
			if mgr != nil {
				managers.Store(sessionID, mgr)
			}
		}(i)
	}
	wg.Wait()

	// Wait for all managers to finish cleanup (they auto-exit via UntilIdleFor).
	managers.Range(func(key, value any) bool {
		mgr := value.(*SessionTurnManager)
		select {
		case <-mgr.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("timeout waiting for manager %s cleanup", key)
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// 3. SessionTurnManager Basic Tests
// ---------------------------------------------------------------------------

func TestSessionTurnManager_CreatedWithCredential(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-mgr-1"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	claim, err := q.ClaimWithCredential(ctx, sessionID, "worker-1", "")
	require.NoError(t, err)
	require.NotNil(t, claim)

	cfg := minimalConfig(t)
	tracker := NewWorkTracker()
	registry := NewSessionManagerRegistry()

	mgr, err := NewSessionTurnManager(ctx, q, sessionID, "worker-1", claim.Credential, "turn-1", "cp-1", cfg, tracker, registry, noopLogFn)
	require.NoError(t, err)
	require.NotNil(t, mgr)
	assert.Equal(t, sessionID, mgr.SessionID())
	assert.Equal(t, "turn-1", mgr.TurnID())
	assert.Equal(t, claim.Credential, mgr.Credential())
}

func TestSessionTurnManager_EmptyCredential(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()

	cfg := minimalConfig(t)
	tracker := NewWorkTracker()
	registry := NewSessionManagerRegistry()

	mgr, err := NewSessionTurnManager(ctx, q, "session", "worker", "", "turn", "cp", cfg, tracker, registry, noopLogFn)
	assert.Error(t, err)
	assert.Nil(t, mgr)
	assert.Contains(t, err.Error(), "credential is required")
}

func TestSessionTurnManager_UpdateCredential(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-cred"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	claim, err := q.ClaimWithCredential(ctx, sessionID, "worker-1", "")
	require.NoError(t, err)
	require.NotNil(t, claim)

	cfg := minimalConfig(t)
	tracker := NewWorkTracker()
	registry := NewSessionManagerRegistry()

	mgr, err := NewSessionTurnManager(ctx, q, sessionID, "worker-1", claim.Credential, "turn-1", "cp-1", cfg, tracker, registry, noopLogFn)
	require.NoError(t, err)

	mgr.UpdateCredential("new-cred")
	assert.Equal(t, "new-cred", mgr.Credential())

	// Empty update should be ignored.
	mgr.UpdateCredential("")
	assert.Equal(t, "new-cred", mgr.Credential())
}

func TestSessionTurnManager_SetCancelledByQueue(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-cancel"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	claim, err := q.ClaimWithCredential(ctx, sessionID, "worker-1", "")
	require.NoError(t, err)

	cfg := minimalConfig(t)
	tracker := NewWorkTracker()
	registry := NewSessionManagerRegistry()

	mgr, err := NewSessionTurnManager(ctx, q, sessionID, "worker-1", claim.Credential, "turn-1", "cp-1", cfg, tracker, registry, noopLogFn)
	require.NoError(t, err)

	assert.False(t, mgr.IsCancelledByQueue())
	assert.Empty(t, mgr.CancelReason())

	mgr.SetCancelledByQueue("admin cancel")
	assert.True(t, mgr.IsCancelledByQueue())
	assert.Equal(t, "admin cancel", mgr.CancelReason())
}

func TestSessionTurnManager_LastMessage(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-msg"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	claim, err := q.ClaimWithCredential(ctx, sessionID, "worker-1", "")
	require.NoError(t, err)

	cfg := minimalConfig(t)
	tracker := NewWorkTracker()
	registry := NewSessionManagerRegistry()

	mgr, err := NewSessionTurnManager(ctx, q, sessionID, "worker-1", claim.Credential, "turn-1", "cp-1", cfg, tracker, registry, noopLogFn)
	require.NoError(t, err)

	assert.Nil(t, mgr.LastMessage())

	msg := &Message{Role: RoleAssistant, Content: "hello"}
	mgr.setLastMessage(msg)
	assert.Equal(t, msg, mgr.LastMessage())
}

func TestSessionTurnManager_DoneChannel(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-done"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	claim, err := q.ClaimWithCredential(ctx, sessionID, "worker-1", "")
	require.NoError(t, err)

	cfg := minimalConfig(t)
	tracker := NewWorkTracker()
	registry := NewSessionManagerRegistry()

	mgr, err := NewSessionTurnManager(ctx, q, sessionID, "worker-1", claim.Credential, "turn-1", "cp-1", cfg, tracker, registry, noopLogFn)
	require.NoError(t, err)

	// Done should not be closed before cleanup.
	select {
	case <-mgr.Done():
		t.Fatal("done should not be closed before cleanup")
	default:
	}

	// After cleanup, done should be closed.
	mgr.Cleanup(ctx)

	select {
	case <-mgr.Done():
		// OK
	case <-time.After(time.Second):
		t.Fatal("done should be closed after cleanup")
	}
}

// ---------------------------------------------------------------------------
// 4. rtc-queue RequeueWork Tests
// ---------------------------------------------------------------------------

func TestRequeueWork(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-requeue"

	// Publish and claim a work item.
	workID := publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)
	claim, err := q.ClaimWithCredential(ctx, sessionID, "worker-1", "")
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.Equal(t, workID, claim.WorkID)

	// Work should be in processing state.
	work, err := q.LoadWork(ctx, workID)
	require.NoError(t, err)
	assert.Equal(t, rtcqueue.StatusProcessing, work.Status)

	// Requeue it.
	err = q.RequeueWork(ctx, workID)
	require.NoError(t, err)

	// Work should be back to pending.
	work, err = q.LoadWork(ctx, workID)
	require.NoError(t, err)
	assert.Equal(t, rtcqueue.StatusPending, work.Status)
}

func TestRequeueWork_NotFound(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()

	err := q.RequeueWork(ctx, "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRequeueWork_NotProcessing(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-requeue-pending"

	// Publish but don't claim — work stays pending.
	workID := publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Requeue should fail because work is not in processing state.
	err := q.RequeueWork(ctx, workID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not in processing state")
}

// ---------------------------------------------------------------------------
// Helper: minimalConfig returns a Config with just enough callbacks for tests.
// ---------------------------------------------------------------------------

func minimalConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		CreateTurn: func(ctx context.Context, sessionID, workID string) (string, error) {
			return "turn-test", nil
		},
		LookupTurn: func(ctx context.Context, sessionID, workID string) (string, error) {
			return "turn-test", nil
		},
		BeginTurn: func(ctx context.Context, turnID string) error { return nil },
		CompleteTurn: func(ctx context.Context, sessionID, turnID string, lastMessage *Message) error {
			return nil
		},
		InterruptTurn: func(ctx context.Context, turnID, interruptID string, interruptInfo any, allInterruptContexts []*InterruptContext) error {
			return nil
		},
		ResumeTurn: func(ctx context.Context, turnID string) error { return nil },
		FailTurn:   func(ctx context.Context, turnID string, err error) error { return nil },
		CancelTurn: func(ctx context.Context, turnID, reason string) error { return nil },
		LoadMessages: func(ctx context.Context, sessionID string) ([]*Message, error) {
			return []*Message{{Role: RoleUser, Content: "test"}}, nil
		},
		CreateTools: func(ctx context.Context, sessionID, turnID string) ([]tool.BaseTool, error) {
			return nil, nil
		},
		CreateAgent: func(ctx context.Context, sessionID, turnID string, tools []tool.BaseTool) (adk.Agent, error) {
			return &mockAgent{name: "test"}, nil
		},
		PublishEvent: func(ctx context.Context, sessionID, turnID string, event *Event) error {
			return nil
		},
		CheckpointStore: &noopCheckpointStore{},
	}
}

// noopCheckpointStore is a no-op checkpoint store for tests.
type noopCheckpointStore struct{}

func (s *noopCheckpointStore) Set(_ context.Context, _ string, _ []byte) error { return nil }
func (s *noopCheckpointStore) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *noopCheckpointStore) Delete(_ context.Context, _ string) error { return nil }
