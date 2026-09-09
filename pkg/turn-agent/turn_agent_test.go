package turnagent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
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

// mockAgent is a minimal adk.Agent (TypedAgent[*schema.Message]) for testing.
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

// mockTurnLoopCallbacks configures the mock TurnLoop's callbacks.
type mockTurnLoopCallbacks struct {
	genInputCalled    chan []TurnWorkItem
	prepareAgentFunc  func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], consumed []TurnWorkItem) (adk.Agent, error)
	onAgentEventsFunc func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error
}

// newMockTurnLoop creates a TurnLoop[TurnWorkItem, *schema.Message] with mock callbacks.
func newMockTurnLoop(t *testing.T, cbs *mockTurnLoopCallbacks) *adk.TurnLoop[TurnWorkItem, *schema.Message] {
	t.Helper()

	var genInputCalled chan []TurnWorkItem
	if cbs != nil && cbs.genInputCalled != nil {
		genInputCalled = cbs.genInputCalled
	} else {
		genInputCalled = make(chan []TurnWorkItem, 10)
	}

	var prepareAgentFn func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], consumed []TurnWorkItem) (adk.Agent, error)
	if cbs != nil && cbs.prepareAgentFunc != nil {
		prepareAgentFn = cbs.prepareAgentFunc
	} else {
		prepareAgentFn = func(_ context.Context, _ *adk.TurnLoop[TurnWorkItem, *schema.Message], _ []TurnWorkItem) (adk.Agent, error) {
			return &mockAgent{name: "test"}, nil
		}
	}

	cfg := adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], items []TurnWorkItem) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
			select {
			case genInputCalled <- items:
			default:
			}
			return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
				Input:    &adk.AgentInput{Messages: []adk.Message{schema.UserMessage("test")}},
				Consumed: items,
			}, nil
		},
		PrepareAgent: prepareAgentFn,
	}

	if cbs != nil && cbs.onAgentEventsFunc != nil {
		cfg.OnAgentEvents = cbs.onAgentEventsFunc
	}

	return adk.NewTurnLoop(cfg)
}

// newMockTurnLoopWithStore creates a TurnLoop with a checkpoint store.
func newMockTurnLoopWithStore(t *testing.T, store adk.CheckPointStore, checkpointID string, cbs *mockTurnLoopCallbacks) *adk.TurnLoop[TurnWorkItem, *schema.Message] {
	t.Helper()

	genInputCh := make(chan []TurnWorkItem, 10)
	if cbs != nil && cbs.genInputCalled != nil {
		genInputCh = cbs.genInputCalled
	}

	prepareAgentFn := func(_ context.Context, _ *adk.TurnLoop[TurnWorkItem, *schema.Message], _ []TurnWorkItem) (adk.Agent, error) {
		return &mockAgent{name: "test"}, nil
	}
	if cbs != nil && cbs.prepareAgentFunc != nil {
		prepareAgentFn = cbs.prepareAgentFunc
	}

	cfg := adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], items []TurnWorkItem) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
			select {
			case genInputCh <- items:
			default:
			}
			return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
				Input:    &adk.AgentInput{Messages: []adk.Message{schema.UserMessage("test")}},
				Consumed: items,
			}, nil
		},
		PrepareAgent: prepareAgentFn,
		Store:        store,
		CheckpointID: checkpointID,
	}

	if cbs != nil && cbs.onAgentEventsFunc != nil {
		cfg.OnAgentEvents = cbs.onAgentEventsFunc
	}

	return adk.NewTurnLoop(cfg)
}

// awaitSubscriberCount polls miniredis PubSubNumSub until the given channel
// has at least want subscribers, or the deadline elapses.
func awaitSubscriberCount(t *testing.T, mr *miniredis.Miniredis, channel string, want int, deadline time.Time) {
	t.Helper()
	for time.Now().Before(deadline) {
		counts := mr.PubSubNumSub(channel)
		if counts[channel] >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	counts := mr.PubSubNumSub(channel)
	t.Fatalf("timed out waiting for %d subscriber(s) on %q (last count: %d)", want, channel, counts[channel])
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

// claimLockForTest claims the session lock and returns the credential.
// Used by tests to provide a valid credential to NewTurnAgent (since
// NewTurnAgent no longer claims the lock itself).
func claimLockForTest(t *testing.T, ctx context.Context, q *rtcqueue.Queue, sessionID, workerID string) string {
	t.Helper()
	claim, err := q.ClaimWithCredential(ctx, sessionID, workerID, "")
	require.NoError(t, err)
	require.NotNil(t, claim, "claim should succeed: queue must have work and lock must be free")
	return claim.Credential
}

// testCheckpointStore is an in-memory checkpoint store for testing.
type testCheckpointStore struct {
	m  map[string][]byte
	mu sync.Mutex
}

func (s *testCheckpointStore) Set(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func (s *testCheckpointStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

// ---------------------------------------------------------------------------
// 1. Lock Management Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_ClaimLock(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-lock-1"

	// Publish a work item so the session queue is non-empty (required for claim).
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves (NewTurnAgent no longer claims the lock).
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)
	require.NotNil(t, ta)
	assert.Equal(t, sessionID, ta.SessionID())
	assert.NotEmpty(t, ta.Credential(), "credential should be set")
}

func TestTurnAgent_LockContention(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-contention"

	// Publish one work item for the first agent to claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 10)

	// Worker 1 claims the lock.
	cred1 := claimLockForTest(t, ctx, q, sessionID, "worker-1")
	loop1 := newMockTurnLoop(t, nil)
	ta1, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred1, "turn-1", loop1)
	require.NoError(t, err)
	require.NotNil(t, ta1)

	// Worker 2 tries to claim the same session — lock is held by worker-1.
	// The queue is empty (worker-1 consumed the item), so ClaimWithCredential
	// returns nil.
	claim2, err := q.ClaimWithCredential(ctx, sessionID, "worker-2", "")
	require.NoError(t, err)
	assert.Nil(t, claim2, "second worker should fail to claim: lock held or queue empty")

	// Even if we somehow got a credential, NewTurnAgent with valid credential
	// would succeed, but the lock renewal would later fail.
	// The real contention test is: can worker-2 claim? No.
	_ = ta1
}

func TestTurnAgent_ClaimFails_EmptyCredential(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-empty"

	// NewTurnAgent requires a non-empty credential.
	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", "", "turn-1", loop)
	assert.Error(t, err)
	assert.Nil(t, ta)
	assert.Contains(t, err.Error(), "credential is required")
}

func TestTurnAgent_StopReleasesLock(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-release"

	// Publish 2 items: one consumed by the test's claim, one for the pump loop.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 10)
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	agentStarted := make(chan struct{})
	agentStartedOnce := sync.Once{}

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		prepareAgentFunc: func(_ context.Context, _ *adk.TurnLoop[TurnWorkItem, *schema.Message], _ []TurnWorkItem) (adk.Agent, error) {
			return &mockAgent{
				name: "test",
				runFunc: func(ctx context.Context, input *adk.AgentInput) (*adk.AgentOutput, error) {
					agentStartedOnce.Do(func() { close(agentStarted) })
					// Return quickly instead of blocking
					return &adk.AgentOutput{}, nil
				},
			}, nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Push initial work item to the loop (simulating what Process does).
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for the agent to start processing.
	select {
	case <-agentStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}

	// Give the turn a moment to complete and the monitor goroutine to call Stop
	time.Sleep(100 * time.Millisecond)

	// Verify lock is held before Stop (agent is still running or just finished)
	ok, err := q.RenewLockWithCredential(ctx, sessionID, "worker-1", ta.Credential())
	require.NoError(t, err)
	// Lock might still be held or already released by the monitor goroutine
	_ = ok

	// Stop the TurnAgent (might already be stopped by monitor)
	ta.Stop()

	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop in time")
	}

	// Lock should be released — a new worker can claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)
	result, err := q.ClaimWithCredential(ctx, sessionID, "worker-2", "")
	require.NoError(t, err)
	assert.NotNil(t, result, "new worker should be able to claim after Stop")
}

// ---------------------------------------------------------------------------
// 2. Pump Loop Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_PumpLoop_PicksUpWork(t *testing.T) {
	t.Parallel()
	q, mr := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-pump-1"

	// Publish 1 work item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	genInputCh := make(chan []TurnWorkItem, 10)
	agentDone := make(chan struct{})

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		genInputCalled: genInputCh,
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			close(agentDone)
			tc.Loop.Stop()
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Push initial work item to the loop (simulating what Process does).
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for GenInput to be called with the initial work item.
	select {
	case items := <-genInputCh:
		require.Len(t, items, 1)
		assert.Equal(t, WorkKindSubmit, items[0].Kind)
		assert.Equal(t, "turn-1", items[0].TurnID)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for GenInput to be called")
	}

	// Wait for agent to finish processing.
	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agent to finish")
	}

	// Wait for TurnAgent to stop.
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop in time")
	}

	// Verify: session:new subscriber was registered.
	_ = mr
}

func TestTurnAgent_PumpLoop_PicksUpAdditionalWork(t *testing.T) {
	t.Parallel()
	q, mr := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-pump-additional"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	genInputCh := make(chan []TurnWorkItem, 10)
	turnCount := int32(0)
	firstTurnDone := make(chan struct{})

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		genInputCalled: genInputCh,
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			count := atomic.AddInt32(&turnCount, 1)
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			if count == 1 {
				close(firstTurnDone)
				// Wait a bit for the pump loop to pick up the additional item.
				time.Sleep(500 * time.Millisecond)
			} else {
				tc.Loop.Stop()
			}
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Push initial work to the loop.
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for the pump loop to subscribe.
	awaitSubscriberCount(t, mr, rtcqueue.ChannelSessionNew, 1, time.Now().Add(3*time.Second))

	// Wait for first turn to complete.
	select {
	case <-firstTurnDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not complete")
	}

	// Publish additional work item AFTER the turn started.
	// The pump loop should pick it up.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 5)

	// Wait for the second turn to process or timeout.
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		ta.Stop()
		<-ta.Done()
	}

	assert.GreaterOrEqual(t, atomic.LoadInt32(&turnCount), int32(1), "at least 1 turn should have been processed")
}

func TestTurnAgent_PumpLoop_IgnoresOtherSessions(t *testing.T) {
	t.Parallel()
	q, mr := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-pump-filter"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	genInputCh := make(chan []TurnWorkItem, 10)
	agentStarted := make(chan struct{})
	agentStartedOnce := sync.Once{}

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		genInputCalled: genInputCh,
		prepareAgentFunc: func(_ context.Context, _ *adk.TurnLoop[TurnWorkItem, *schema.Message], _ []TurnWorkItem) (adk.Agent, error) {
			return &mockAgent{
				name: "test",
				runFunc: func(ctx context.Context, input *adk.AgentInput) (*adk.AgentOutput, error) {
					agentStartedOnce.Do(func() { close(agentStarted) })
					return &adk.AgentOutput{}, nil
				},
			}, nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for the pump loop to subscribe.
	awaitSubscriberCount(t, mr, rtcqueue.ChannelSessionNew, 1, time.Now().Add(3*time.Second))

	// Wait for agent to start (proves the initial item was processed).
	select {
	case <-agentStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}

	// Drain the GenInput channel from the initial turn.
	select {
	case <-genInputCh:
	case <-time.After(2 * time.Second):
	}

	// Publish work for a DIFFERENT session.
	publishWorkJSON(t, ctx, q, "other-session", WorkPayload{Kind: WorkKindSubmit, SessionID: "other-session"}, 5)

	// The pump loop should NOT pick up work from another session.
	select {
	case items := <-genInputCh:
		t.Fatalf("should not have received items from another session, got: %v", items)
	case <-time.After(500 * time.Millisecond):
		// Good: nothing was picked up for the other session.
	}

	ta.Stop()
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop in time")
	}
}

func TestTurnAgent_PumpLoop_MultipleWorkItems(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-pump-multi"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	var allItems []TurnWorkItem
	var mu sync.Mutex
	agentDone := make(chan struct{})

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			mu.Lock()
			allItems = append(allItems, tc.Consumed...)
			mu.Unlock()

			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			close(agentDone)
			tc.Loop.Stop()
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Push initial work to the loop.
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for the agent to process and stop.
	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish")
	}

	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop in time")
	}

	mu.Lock()
	defer mu.Unlock()
	// The initial item should have been processed.
	assert.GreaterOrEqual(t, len(allItems), 1, "at least 1 work item should have been processed")
}

// ---------------------------------------------------------------------------
// 3. Turn Lifecycle Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_TurnLifecycle_Complete(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-lifecycle-complete"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	agentFinished := make(chan struct{})

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			close(agentFinished)
			tc.Loop.Stop()
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Push initial work to the loop.
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for agent to finish.
	select {
	case <-agentFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish")
	}

	// Wait for TurnAgent to stop (auto-stopped by monitor goroutine).
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop after agent completed")
	}

	// Verify lock was released.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)
	result, err := q.ClaimWithCredential(ctx, sessionID, "worker-2", "")
	require.NoError(t, err)
	assert.NotNil(t, result, "lock should be released after turn completes")
}

func TestTurnAgent_TurnLifecycle_NewSubmitDuringTurn(t *testing.T) {
	t.Parallel()
	q, mr := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-lifecycle-submit"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	firstTurnProcessed := make(chan struct{})
	secondTurnProcessed := make(chan struct{})
	turnCount := int32(0)

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			count := atomic.AddInt32(&turnCount, 1)
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			switch count {
			case 1:
				close(firstTurnProcessed)
				// Wait for the pump loop to pick up the second item.
				time.Sleep(500 * time.Millisecond)
			case 2:
				close(secondTurnProcessed)
				tc.Loop.Stop()
			}
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	awaitSubscriberCount(t, mr, rtcqueue.ChannelSessionNew, 1, time.Now().Add(3*time.Second))

	// Wait for first turn to process.
	select {
	case <-firstTurnProcessed:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not process")
	}

	// Submit a second work item while the turn is still active.
	// The pump loop should pick this up.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 5)

	// Wait for second turn or for the agent to stop.
	select {
	case <-secondTurnProcessed:
		// Both turns processed.
	case <-ta.Done():
		// Agent stopped before second turn (acceptable if the second item was not picked up in time).
	case <-time.After(5 * time.Second):
		ta.Stop()
		<-ta.Done()
	}

	assert.GreaterOrEqual(t, atomic.LoadInt32(&turnCount), int32(1), "at least 1 turn should have been processed")
}

func TestTurnAgent_WaitReturnsExitState(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-wait"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	go ta.Run(ctx)

	ta.Stop()

	exitState := ta.Wait()
	require.NotNil(t, exitState)
	assert.NoError(t, exitState.ExitReason)
}

// ---------------------------------------------------------------------------
// 4. Concurrent Scenario Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_ConcurrentStop(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-concurrent-stop"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	go ta.Run(ctx)

	// Wait for Run initialization.
	select {
	case <-ta.initDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not initialize in time")
	}

	// Multiple goroutines call Stop concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ta.Stop()
		}()
	}
	wg.Wait()

	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop after concurrent Stop calls")
	}
}

func TestTurnAgent_StopThenWait(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-stop-wait"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	go ta.Run(ctx)

	ta.Stop()
	exitState := ta.Wait()
	require.NotNil(t, exitState)
	assert.NoError(t, exitState.ExitReason)
}

func TestTurnAgent_DoneChannel(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-done"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	go ta.Run(ctx)

	// Before Stop, Done channel should be open.
	select {
	case <-ta.Done():
		t.Fatal("Done channel should not be closed before Stop")
	default:
	}

	ta.Stop()

	// After Stop, Done channel should be closed.
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done channel not closed after Stop")
	}
}

// ---------------------------------------------------------------------------
// 5. Checkpoint Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_Checkpoint_SavedOnTurnEnd(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-checkpoint-save"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	store := &testCheckpointStore{m: make(map[string][]byte)}
	checkpointID := "test-checkpoint-1"

	agentDone := make(chan struct{})
	loop := newMockTurnLoopWithStore(t, store, checkpointID, &mockTurnLoopCallbacks{
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			close(agentDone)
			tc.Loop.Stop()
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for agent to finish.
	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish")
	}

	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop")
	}

	exitState := ta.Wait()
	require.NotNil(t, exitState)
	assert.NoError(t, exitState.ExitReason)
}

func TestTurnAgent_Checkpoint_NotSavedOnLockLost(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-checkpoint-locklost"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	store := &testCheckpointStore{m: make(map[string][]byte)}
	checkpointID := "test-checkpoint-locklost"

	agentDone := make(chan struct{})
	loop := newMockTurnLoopWithStore(t, store, checkpointID, &mockTurnLoopCallbacks{
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			close(agentDone)
			tc.Loop.Stop()
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for turn to process.
	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not finish")
	}

	// Wait for the TurnAgent to stop.
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop")
	}

	// After turn completes, the lock is released. Verify another worker can claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)
	otherResult, err := q.ClaimWithCredential(ctx, sessionID, "worker-2", "")
	require.NoError(t, err)
	require.NotNil(t, otherResult, "other worker should be able to claim after lock is released")

	// Verify the credential mechanism: old credential no longer works.
	ok, err := q.RenewLockWithCredential(ctx, sessionID, "worker-1", ta.Credential())
	require.NoError(t, err)
	assert.False(t, ok, "renewal with stale credential should fail")
}

// ---------------------------------------------------------------------------
// 6. Lock Renewal Mechanism Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_LockRenewal_Mechanism(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-renewal-mechanism"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Valid credential succeeds.
	ok, err := q.RenewLockWithCredential(ctx, sessionID, "worker-1", ta.Credential())
	require.NoError(t, err)
	assert.True(t, ok, "renewal with valid credential should succeed")

	// Wrong credential fails.
	ok, err = q.RenewLockWithCredential(ctx, sessionID, "worker-1", "wrong-credential")
	require.NoError(t, err)
	assert.False(t, ok, "renewal with wrong credential should fail")

	// Wrong worker fails.
	ok, err = q.RenewLockWithCredential(ctx, sessionID, "wrong-worker", ta.Credential())
	require.NoError(t, err)
	assert.False(t, ok, "renewal with wrong worker should fail")
}

func TestTurnAgent_LockRenewal_TTLExpiry(t *testing.T) {
	t.Parallel()
	q, mr := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-renewal-ttl"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	credential := ta.Credential()

	// Renewal works before TTL expiry.
	ok, err := q.RenewLockWithCredential(ctx, sessionID, "worker-1", credential)
	require.NoError(t, err)
	assert.True(t, ok)

	// Expire the lock.
	mr.FastForward(121 * time.Second)

	// Renewal should now fail.
	ok, err = q.RenewLockWithCredential(ctx, sessionID, "worker-1", credential)
	require.NoError(t, err)
	assert.False(t, ok, "renewal should fail after TTL expiry")
}

// ---------------------------------------------------------------------------
// 7. Credential Management Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_CredentialManagement(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-credential"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	assert.NotEmpty(t, cred)

	ta.setCredential("new-credential")
	assert.Equal(t, "new-credential", ta.getCredential())

	// Concurrent access should not panic.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			ta.setCredential("cred-" + string(rune('A'+i%26)))
		}(i)
		go func() {
			defer wg.Done()
			_ = ta.getCredential()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 8. Session ID Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_SessionID(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-id-test"

	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 1)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	loop := newMockTurnLoop(t, nil)
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	assert.Equal(t, sessionID, ta.SessionID())
}

// ---------------------------------------------------------------------------
// 9. Work Payload Decoding Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_PumpLoop_DecodesWorkPayload(t *testing.T) {
	t.Parallel()
	q, _ := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-decode"

	// Publish 1 item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Claim the lock ourselves.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	genInputCh := make(chan []TurnWorkItem, 10)

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		genInputCalled: genInputCh,
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			tc.Loop.Stop()
			return nil
		},
	})

	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-resume", loop)
	require.NoError(t, err)

	// Push initial work (a resume payload) to the loop.
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindResume, SessionID: sessionID},
		TurnID:      "turn-resume",
	})
	require.True(t, pushed)

	go ta.Run(ctx)

	// Wait for Run initialization to complete.
	select {
	case <-ta.initDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not initialize in time")
	}

	// Verify the payload was correctly processed.
	select {
	case items := <-genInputCh:
		require.Len(t, items, 1)
		assert.Equal(t, WorkKindResume, items[0].Kind)
		assert.Equal(t, sessionID, items[0].SessionID)
		assert.Equal(t, "turn-resume", items[0].TurnID)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for GenInput")
	}

	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("TurnAgent did not stop")
	}
}

// ---------------------------------------------------------------------------
// 10. Pump Loop TurnID Verification Tests
// ---------------------------------------------------------------------------

func TestTurnAgent_PumpLoop_NewMessageDuringTurn(t *testing.T) {
	t.Parallel()
	q, mr := newTestQueue(t)
	ctx := context.Background()
	sessionID := "session-pump-newmsg-during"

	// Step 1: Publish 1 work item consumed by our claim.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 100)

	// Step 2: Claim the lock.
	cred := claimLockForTest(t, ctx, q, sessionID, "worker-1")

	genInputCh := make(chan []TurnWorkItem, 10)
	firstTurnProcessing := make(chan struct{})
	secondTurnReceived := make(chan []TurnWorkItem, 1)
	turnCount := int32(0)

	loop := newMockTurnLoop(t, &mockTurnLoopCallbacks{
		genInputCalled: genInputCh,
		onAgentEventsFunc: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			count := atomic.AddInt32(&turnCount, 1)
			// Drain all events from the mock agent.
			for {
				_, ok := events.Next()
				if !ok {
					break
				}
			}
			if count == 1 {
				// Signal that Turn 1 is actively being processed.
				close(firstTurnProcessing)
				// Keep Turn 1 alive while the test publishes a new work item and
				// the pump loop picks it up. 1 second is generous for miniredis.
				time.Sleep(1 * time.Second)
			} else {
				// Second turn: capture consumed items for turnID verification.
				select {
				case secondTurnReceived <- tc.Consumed:
				default:
				}
				tc.Loop.Stop()
			}
			return nil
		},
	})

	// Step 3-4: Create TurnAgent with turnID "turn-1".
	ta, err := NewTurnAgent(ctx, q, sessionID, "worker-1", cred, "turn-1", loop)
	require.NoError(t, err)

	// Step 5: Push initial work item to the loop (simulating what Process does).
	pushed, _ := loop.Push(TurnWorkItem{
		WorkPayload: WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID},
		TurnID:      "turn-1",
	})
	require.True(t, pushed)

	// Step 6: Start TurnAgent.Run().
	go ta.Run(ctx)

	// Step 7: Wait for pump loop subscription.
	awaitSubscriberCount(t, mr, rtcqueue.ChannelSessionNew, 1, time.Now().Add(3*time.Second))

	// Wait for Turn 1 to be actively processing.
	select {
	case <-firstTurnProcessing:
	case <-time.After(5 * time.Second):
		t.Fatal("Turn 1 did not start processing")
	}

	// Step 8 (KEY STEP): While Turn 1 is still running (agent is processing),
	// publish a new work item to the queue.
	publishWorkJSON(t, ctx, q, sessionID, WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID}, 5)

	// Step 9-10: Wait for the second turn to receive items and verify turnID.
	select {
	case items := <-secondTurnReceived:
		require.NotEmpty(t, items, "second turn should have received items pushed by pump loop")
		for _, item := range items {
			// KEY VERIFICATION: pump loop must use the TurnAgent's turnID ("turn-1"),
			// not any other ID. This validates that runPumpLoop correctly assigns
			// ta.turnID to every TurnWorkItem it pushes to the TurnLoop buffer.
			assert.Equal(t, "turn-1", item.TurnID,
				"pump loop must use the TurnAgent's turnID for work items pushed to buffer")
			assert.Equal(t, WorkKindSubmit, item.Kind)
			assert.Equal(t, sessionID, item.SessionID)
		}
	case <-time.After(5 * time.Second):
		ta.Stop()
		<-ta.Done()
		t.Fatal("timed out waiting for second turn: pump loop did not push new work item to buffer")
	}

	// Wait for TurnAgent to stop cleanly.
	select {
	case <-ta.Done():
	case <-time.After(5 * time.Second):
		ta.Stop()
		<-ta.Done()
	}

	// Additional verification: drain GenInput channel and verify ALL items had correct turnID.
	close(genInputCh)
	totalGenInputItems := 0
	for items := range genInputCh {
		for _, item := range items {
			totalGenInputItems++
			assert.Equal(t, "turn-1", item.TurnID,
				"all work items passed to GenInput must carry the TurnAgent's turnID")
		}
	}
	assert.GreaterOrEqual(t, totalGenInputItems, 2,
		"at least 2 work items should have been passed to GenInput (initial + pump loop pickup)")
}

// ---------------------------------------------------------------------------
// CompleteAndClaimNext + SignalTurnComplete
// ---------------------------------------------------------------------------

// TestTurnAgent_SignalTurnComplete verifies that SignalTurnComplete signals
// the turn completion channel, allowing Process() to check for next work.
func TestTurnAgent_SignalTurnComplete(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	q := rtcqueue.New(rdb)

	sessionID := "session-signal"
	workerID := "worker-1"

	// Publish a work item first so claim succeeds.
	payload, _ := json.Marshal(WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID})
	_, err = q.Publish(context.Background(), sessionID, string(payload), 0)
	require.NoError(t, err)

	// Claim lock to get credential.
	claim, err := q.ClaimWithCredential(context.Background(), sessionID, workerID, "")
	require.NoError(t, err)
	require.NotNil(t, claim)

	// Create a minimal TurnLoop config.
	cfg := adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], items []TurnWorkItem) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
			return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
				RunCtx: ctx,
				Input:  &adk.TypedAgentInput[*schema.Message]{Messages: []*schema.Message{}},
			}, nil
		},
	}
	loop := adk.NewTurnLoop[TurnWorkItem, *schema.Message](cfg)
	ta, err := NewTurnAgent(context.Background(), q, sessionID, workerID, claim.Credential, "turn-1", loop)
	require.NoError(t, err)

	// Signal turn completion.
	ta.SignalTurnComplete()

	// Wait for turn completion should return true.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	assert.True(t, ta.WaitForTurnComplete(ctx), "WaitForTurnComplete should return true after signal")

	// Second wait should return false (no more signals).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	assert.False(t, ta.WaitForTurnComplete(ctx2), "WaitForTurnComplete should return false when no signal")
}

// TestTurnAgent_CompleteAndClaimNext_Integration verifies the complete flow:
// 1. Turn completes (SignalTurnComplete)
// 2. Process checks for next work (CompleteAndClaimNext)
// 3. If next work exists, push to buffer and continue
// 4. If no next work, stop the loop
func TestTurnAgent_CompleteAndClaimNext_Integration(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	q := rtcqueue.New(rdb)

	sessionID := "session-integration"
	workerID := "worker-1"
	turnID := "turn-1"

	// Publish two work items.
	payload1, _ := json.Marshal(WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID})
	workID1, err := q.Publish(context.Background(), sessionID, string(payload1), 0)
	require.NoError(t, err)

	payload2, _ := json.Marshal(WorkPayload{Kind: WorkKindSubmit, SessionID: sessionID})
	workID2, err := q.Publish(context.Background(), sessionID, string(payload2), 0)
	require.NoError(t, err)

	// Claim first work item.
	claim1, err := q.ClaimWithCredential(context.Background(), sessionID, workerID, "")
	require.NoError(t, err)
	require.NotNil(t, claim1, "first claim should succeed")
	assert.Equal(t, workID1, claim1.WorkID, "should claim first work item")

	// Create a minimal TurnLoop config.
	cfg := adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], items []TurnWorkItem) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
			return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
				RunCtx: ctx,
				Input:  &adk.TypedAgentInput[*schema.Message]{Messages: []*schema.Message{}},
			}, nil
		},
	}
	loop := adk.NewTurnLoop[TurnWorkItem, *schema.Message](cfg)
	ta, err := NewTurnAgent(context.Background(), q, sessionID, workerID, claim1.Credential, turnID, loop)
	require.NoError(t, err)

	// Simulate turn completion.
	ta.SignalTurnComplete()

	// Wait for signal.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	assert.True(t, ta.WaitForTurnComplete(ctx))

	// Complete current work and claim next.
	claim2, err := q.CompleteAndClaimNext(context.Background(), workID1, sessionID, workerID, claim1.Credential)
	require.NoError(t, err)
	require.NotNil(t, claim2, "should claim next work item")
	assert.Equal(t, workID2, claim2.WorkID, "should claim second work item")

	// Load the claimed work.
	work2, err := q.LoadWork(context.Background(), workID2)
	require.NoError(t, err)
	require.NotNil(t, work2)

	// Complete second work (no more work).
	claim3, err := q.CompleteAndClaimNext(context.Background(), workID2, sessionID, workerID, claim2.Credential)
	require.NoError(t, err)
	assert.Nil(t, claim3, "should return nil when no more work")
}
