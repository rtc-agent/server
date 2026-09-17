package turnagent

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// claimRetryTimeout is how long GetOrCreate waits for an old manager's done
// channel before retrying the claim.
const claimRetryTimeout = 5 * time.Second

// SessionManagerRegistry is a thread-safe map of sessionID to *SessionTurnManager.
//
// It provides GetOrCreate, which atomically looks up or creates a manager for
// a session. When an old manager exists but its loop has stopped, GetOrCreate
// removes it and creates a new one. When ClaimWithCredential fails because the
// old manager hasn't released the lock yet, GetOrCreate waits for the old
// manager's done channel and retries.
type SessionManagerRegistry struct {
	mu       sync.RWMutex
	managers map[string]*SessionTurnManager

	// removedManagers tracks recently removed managers that may still be
	// running cleanup (holding session locks). GetOrCreate uses these
	// references to wait for cleanup before retrying claims.
	removedManagers map[string]*SessionTurnManager
}

// NewSessionManagerRegistry creates an empty registry.
func NewSessionManagerRegistry() *SessionManagerRegistry {
	return &SessionManagerRegistry{
		managers:        make(map[string]*SessionTurnManager),
		removedManagers: make(map[string]*SessionTurnManager),
	}
}

// Get returns the manager for the given session, or nil if not found.
func (r *SessionManagerRegistry) Get(sessionID string) *SessionTurnManager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.managers[sessionID]
}

// scheduleCleanupTracking launches a goroutine that waits for mgr to finish
// cleanup (done channel closed) and then removes it from removedManagers.
// A timeout prevents goroutine accumulation if mgr.Done() is never closed
// (e.g., manager cleanup hangs on unresponsive Redis).
func (r *SessionManagerRegistry) scheduleCleanupTracking(sessionID string, mgr *SessionTurnManager, logPrefix string) {
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				mgr.log(context.Background(), LogLevelError, logPrefix+"_panic", map[string]any{
					"session_id": mgr.sessionID,
					"panic":      fmt.Sprintf("%v", rv),
					"stack":      string(debug.Stack()),
				})
			}
		}()
		select {
		case <-mgr.Done():
		case <-time.After(5 * time.Minute):
			mgr.log(context.Background(), LogLevelWarn, logPrefix+"_timeout", map[string]any{
				"session_id": mgr.sessionID,
			})
		}
		r.mu.Lock()
		if r.removedManagers[sessionID] == mgr {
			delete(r.removedManagers, sessionID)
		}
		r.mu.Unlock()
	}()
}

// Remove deletes the manager for the given session.
// The manager is tracked in removedManagers so that GetOrCreate can wait
// for its cleanup to complete before retrying claims.
// A self-cleanup goroutine is launched to automatically remove the entry
// from removedManagers once the manager's done channel is closed, preventing
// memory leaks when no one calls waitForRemovedManagers.
func (r *SessionManagerRegistry) Remove(sessionID string) {
	r.mu.Lock()
	if mgr, ok := r.managers[sessionID]; ok {
		delete(r.managers, sessionID)
		r.removedManagers[sessionID] = mgr
		r.mu.Unlock()

		r.scheduleCleanupTracking(sessionID, mgr, "session_registry.cleanup")
	} else {
		r.mu.Unlock()
	}
}

// GetOrCreate returns the existing manager for the session, or creates a new one.
//
// If a manager exists (regardless of whether its loop is still running), it is
// returned with isNew=false. The caller must check health externally (e.g., via
// Push) and use Replace when the loop has stopped.
//
// If no manager exists, it creates a new one. The behavior depends on whether
// a credential was provided:
//   - If initialCredential is non-empty (Worker already claimed work), use it
//     directly to create the manager without calling ClaimWithCredential.
//   - If initialCredential is empty, call ClaimWithCredential to claim the lock.
//     If claim fails because the old manager's lock hasn't been released yet,
//     wait for the old manager's done channel (up to claimRetryTimeout) and retry.
//
// The returned boolean is true when a new manager was created.
//
// For the stale-loop replacement path (Push fails on existing manager), use
// Replace instead.
func (r *SessionManagerRegistry) GetOrCreate(
	ctx context.Context,
	queue *rtcqueue.Queue,
	sessionID, workerID, turnID, checkpointID string,
	initialCredential string,
	cfg Config,
	logFn func(ctx context.Context, level LogLevel, msg string, fields map[string]any),
) (*SessionTurnManager, bool, error) {
	// 1. Fast path: read-lock check for existing manager, avoiding blocking other sessions' read/write.
	r.mu.RLock()
	existing := r.managers[sessionID]
	r.mu.RUnlock()
	if existing != nil {
		return existing, false, nil
	}

	// 2. No existing manager: need to create a new one. First determine the credential.
	var credential string

	if initialCredential != "" {
		// Worker already claimed work and obtained the credential.
		// Use it directly without calling ClaimWithCredential (which would try to
		// pop work from the queue, but the work is already claimed).
		credential = initialCredential
	} else {
		// No credential provided. This means we need to claim the lock ourselves.
		// This path is used when GetOrCreate is called directly (not via Worker).
		// Redis operations execute without holding the registry lock, avoiding blocking other sessions.
		claim, err := queue.ClaimWithCredential(ctx, sessionID, workerID, "")
		if err != nil {
			return nil, false, fmt.Errorf("turnagent: claim: %w", err)
		}
		if claim == nil {
			// Claim failed. This may be because a recently removed manager's cleanup
			// hasn't finished yet (lock still held). Wait for removed managers' done
			// channels, then retry once (Fix 2 from architecture doc).
			r.waitForRemovedManagers(ctx, sessionID, claimRetryTimeout)

			claim, err = queue.ClaimWithCredential(ctx, sessionID, workerID, "")
			if err != nil {
				return nil, false, fmt.Errorf("turnagent: claim retry: %w", err)
			}
			if claim == nil {
				return nil, false, fmt.Errorf("turnagent: claim returned nil: session lock held by another worker or queue empty")
			}
		}
		credential = claim.Credential
	}

	// 3. Write-lock to register the new manager (double-check to prevent concurrent creation).
	r.mu.Lock()
	if existing := r.managers[sessionID]; existing != nil {
		r.mu.Unlock()
		return existing, false, nil
	}

	tracker := NewWorkTracker()

	mgr, err := NewSessionTurnManager(ctx, queue, sessionID, workerID, credential, turnID, checkpointID, cfg, tracker, r, logFn)
	if err != nil {
		r.mu.Unlock()
		return nil, false, fmt.Errorf("turnagent: create manager: %w", err)
	}

	r.managers[sessionID] = mgr
	r.mu.Unlock()

	// Start the manager's loop and lock renewal.
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				mgr.log(ctx, LogLevelError, "session-registry-run-panic", map[string]any{
					"session_id": mgr.sessionID,
					"panic":      fmt.Sprintf("%v", rv),
					"stack":      string(debug.Stack()),
				})
			}
		}()
		mgr.Run(ctx)
	}()

	return mgr, true, nil
}

// waitForRemovedManagers waits for all removed managers of the given session
// to finish their cleanup (done channel closed), up to the given timeout.
// The caller must NOT hold r.mu.
func (r *SessionManagerRegistry) waitForRemovedManagers(ctx context.Context, sessionID string, timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		r.mu.RLock()
		oldMgr := r.removedManagers[sessionID]
		r.mu.RUnlock()

		if oldMgr == nil {
			return
		}

		select {
		case <-oldMgr.Done():
			// Old manager finished cleanup. Clean up the tracking entry.
			r.mu.Lock()
			if r.removedManagers[sessionID] == oldMgr {
				delete(r.removedManagers, sessionID)
			}
			r.mu.Unlock()
			return
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Replace removes the old manager and creates a new one. Used when Push fails
// on an existing manager (loop stopped).
//
// The caller must hold no locks; Replace acquires the write lock internally.
// If the old manager's lock is still held, it waits for the old manager's done
// channel and then uses the provided credential (from the Worker's claim) to
// create the new manager.
//
// Returns the new manager. The boolean is always true — the caller is now the
// owner of the session's turn lifecycle (analogous to isNew=true from
// GetOrCreate).
func (r *SessionManagerRegistry) Replace(
	ctx context.Context,
	queue *rtcqueue.Queue,
	sessionID, workerID, turnID, checkpointID string,
	oldMgr *SessionTurnManager,
	credential string, // credential from Worker's claim (work.Credential)
	cfg Config,
	logFn func(ctx context.Context, level LogLevel, msg string, fields map[string]any),
) (*SessionTurnManager, bool, error) {
	r.mu.Lock()

	// Remove old manager if it's still the one we expect.
	current := r.managers[sessionID]
	oldMgrTracked := false
	if current == oldMgr {
		delete(r.managers, sessionID)
		r.removedManagers[sessionID] = oldMgr
		oldMgrTracked = true
	}

	// Self-cleanup: automatically remove from removedManagers once
	// the old manager has fully stopped, preventing memory leaks
	// when no one calls waitForRemovedManagers.
	// Use a timeout to prevent goroutine accumulation if oldMgr.Done()
	// is never closed (e.g., manager cleanup hangs on unresponsive Redis).
	if oldMgrTracked {
		r.scheduleCleanupTracking(sessionID, oldMgr, "session_registry.replace_cleanup")
	}

	// Wait for the old manager's cleanup to complete before creating a new one.
	// The old manager's cleanup will call ReleaseSession which releases the lock.
	// We must wait for this to complete to avoid a split-brain scenario where
	// the new manager starts while the old manager's cleanup is still running.
	//
	// If credential is provided (Worker already claimed work), we can use it
	// directly after waiting. If no credential is provided, we need to claim
	// the lock ourselves after the old manager releases it.
	if credential == "" {
		// No credential provided. Need to claim the lock ourselves.
		// Try to claim with an empty credential. This intentionally generates a new
		// UUID that won't match the existing lock, so the first claim attempt always
		// returns nil when the old lock is still held. This is by design: we MUST wait
		// for the old manager's cleanup to complete (via oldMgr.Done()) before creating
		// a new manager.
		//
		// Release the mutex before the Redis call to avoid blocking all other sessions
		// during a potentially slow network operation (Bug 25 fix).
		r.mu.Unlock()
		claim, err := queue.ClaimWithCredential(ctx, sessionID, workerID, "")
		if err != nil {
			return nil, false, fmt.Errorf("turnagent: claim: %w", err)
		}
		if claim == nil {
			// Lock still held by old manager. Wait for it to finish.
			oldDone := oldMgr.Done()

			select {
			case <-oldDone:
				// Old manager finished cleanup. Retry.
			case <-time.After(claimRetryTimeout):
				return nil, false, fmt.Errorf("turnagent: timeout waiting for old manager cleanup")
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}

			// Retry claim after old manager cleanup. The old lock has been released
			// (normal case), so this creates a new lock with a fresh credential.
			// In the lockLost case, another worker holds the lock and this correctly
			// fails.
			claim, err = queue.ClaimWithCredential(ctx, sessionID, workerID, "")
			if err != nil {
				return nil, false, fmt.Errorf("turnagent: claim retry: %w", err)
			}
			if claim == nil {
				return nil, false, fmt.Errorf("turnagent: claim retry returned nil")
			}
		}
		credential = claim.Credential

		// Re-acquire mutex for manager registration (double-check for TOCTOU).
		r.mu.Lock()
		if existing := r.managers[sessionID]; existing != nil {
			r.mu.Unlock()
			return existing, false, nil
		}
	} else {
		// Credential provided (Worker already claimed work).
		// Wait for old manager to finish cleanup.
		oldDone := oldMgr.Done()
		r.mu.Unlock()

		select {
		case <-oldDone:
			// Old manager finished cleanup.
		case <-time.After(claimRetryTimeout):
			return nil, false, fmt.Errorf("turnagent: timeout waiting for old manager cleanup")
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}

		r.mu.Lock()
		// Double-check: another goroutine may have created a manager via GetOrCreate
		// while we were waiting for oldDone (TOCTOU fix).
		if existing := r.managers[sessionID]; existing != nil {
			r.mu.Unlock()
			return existing, false, nil
		}
	}

	tracker := NewWorkTracker()

	mgr, err := NewSessionTurnManager(ctx, queue, sessionID, workerID, credential, turnID, checkpointID, cfg, tracker, r, logFn)
	if err != nil {
		r.mu.Unlock()
		return nil, false, fmt.Errorf("turnagent: create manager: %w", err)
	}

	r.managers[sessionID] = mgr
	r.mu.Unlock()

	// Start the manager's loop and lock renewal.
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				mgr.log(ctx, LogLevelError, "session-registry-run-panic", map[string]any{
					"session_id": mgr.sessionID,
					"panic":      fmt.Sprintf("%v", rv),
					"stack":      string(debug.Stack()),
					"source":     "Replace",
				})
			}
		}()
		mgr.Run(ctx)
	}()

	return mgr, true, nil
}
