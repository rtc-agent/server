package turnagent

import "sync"

// WorkTracker tracks per-work-item completion signals.
//
// When a work item is pushed to a SessionTurnManager's loop, Process() registers
// the workID with the tracker and blocks on the returned channel. When the loop's
// OnAgentEvents completes the work, it calls Complete(workID) to unblock Process().
//
// WorkTracker is safe for concurrent use.
type WorkTracker struct {
	mu       sync.Mutex
	pending  map[string]chan struct{}
	allDone  bool // set by CompleteAll; once true, future Registers return closed channels
}

// NewWorkTracker creates an empty WorkTracker.
func NewWorkTracker() *WorkTracker {
	return &WorkTracker{
		pending: make(map[string]chan struct{}),
	}
}

// Register adds a workID to the tracker and returns a channel that will be
// closed when the work is completed. If the workID is already registered,
// the existing channel is returned.
//
// If CompleteAll has already been called (manager shut down), returns an
// already-closed channel. The caller can use IsAbandoned to check whether
// the returned channel represents actual completion or shutdown.
func (t *WorkTracker) Register(workID string) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	if ch, ok := t.pending[workID]; ok {
		return ch
	}

	// If the tracker has been abandoned (CompleteAll called), return a
	// pre-closed channel so the caller unblocks immediately.
	if t.allDone {
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	ch := make(chan struct{})
	t.pending[workID] = ch
	return ch
}

// Complete closes the channel for the given workID, unblocking any goroutine
// waiting on it. If the workID is not registered, Complete is a no-op.
// Complete is idempotent: calling it multiple times for the same workID is safe.
//
// Complete removes the entry from the pending map, distinguishing it from
// CompleteAll which leaves entries in the map (signaling abandonment).
//
// If CompleteAll has already been called, Complete simply removes the entry
// (the channel was already closed by CompleteAll).
func (t *WorkTracker) Complete(workID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if ch, ok := t.pending[workID]; ok {
		delete(t.pending, workID)
		if !t.allDone {
			close(ch)
		}
		// If allDone is true, the channel was already closed by CompleteAll.
	}
}

// CompleteAll closes all pending channels. Used during cleanup to unblock
// all waiting Process() calls when the manager is shutting down (e.g., due
// to cancellation or lock loss).
// CompleteAll is idempotent: calling it multiple times is safe.
//
// Unlike Complete, CompleteAll does NOT remove entries from the pending map.
// This allows callers to distinguish "work was completed normally" (Complete
// removes the entry) from "work was abandoned due to shutdown" (CompleteAll
// leaves the entry). Use IsAbandoned to check which case applies.
func (t *WorkTracker) CompleteAll() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.allDone {
		return // already done — idempotent
	}
	t.allDone = true

	for _, ch := range t.pending {
		close(ch)
		// NOTE: intentionally do NOT delete from t.pending.
		// The presence of the key after the channel is closed signals
		// that the work was abandoned, not completed.
	}
}

// IsAbandoned reports whether the given workID was abandoned (i.e., its
// completion channel was closed by CompleteAll during manager shutdown,
// rather than by Complete after the work was actually processed).
//
// Returns false if the workID was completed normally or was never registered.
func (t *WorkTracker) IsAbandoned(workID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.pending[workID]
	return ok
}
