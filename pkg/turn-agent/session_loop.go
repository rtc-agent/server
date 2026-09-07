package turnagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// TurnWorkItem wraps WorkPayload with turnID for long-running loop processing.
// This allows the loop to know which turn is being processed.
type TurnWorkItem struct {
	WorkPayload
	TurnID string
}

// SessionLoop manages a long-running TurnLoop for a single session.
type SessionLoop struct {
	sessionID string
	loop      *adk.TurnLoop[TurnWorkItem, *schema.Message]
	cancel    context.CancelFunc
	done      chan struct{} // closed when loop exits

	mu         sync.Mutex
	waitCh     chan error    // channel for waiting turn to complete
	lastActive time.Time

	// lastMessage stores the last assistant message from the most recent turn.
	// Used by Sub Agent support to return the result to the parent session.
	// Reset at the start of each turn, updated during the turn.
	lastMessage *Message

	// recreateTurn is the callback to create a new TurnLoop, reusing the existing SessionLoop.
	// Used to recreate the TurnLoop after it exits (e.g., due to interrupt).
	// The callback receives the existing SessionLoop so that the new TurnLoop's OnAgentEvents
	// calls NotifyTurnDone on the same SessionLoop that PushAndWait is waiting on.
	recreateTurn func(sl *SessionLoop) (*adk.TurnLoop[TurnWorkItem, *schema.Message], context.CancelFunc)
}

// SetLastMessage sets the last message for the current turn.
func (sl *SessionLoop) SetLastMessage(msg *Message) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.lastMessage = msg
}

// GetLastMessage returns the last message from the most recent turn.
func (sl *SessionLoop) GetLastMessage() *Message {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return sl.lastMessage
}

// ResetLastMessage resets the last message at the start of a new turn.
func (sl *SessionLoop) ResetLastMessage() {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.lastMessage = nil
}

// SessionLoopRegistry manages session→loop mappings.
type SessionLoopRegistry struct {
	mu     sync.RWMutex
	loops  map[string]*SessionLoop
	onStop func(sessionID string)
}

// NewSessionLoopRegistry creates a new registry.
func NewSessionLoopRegistry(onStop func(string)) *SessionLoopRegistry {
	return &SessionLoopRegistry{
		loops:  make(map[string]*SessionLoop),
		onStop: onStop,
	}
}

// GetOrCreate returns the existing loop for a session, or creates a new one.
// The createLoop callback receives the SessionLoop to be used and should return the TurnLoop and cancel func.
// The SessionLoop's loop field will be started automatically with an independent context
// (not tied to the caller's context), so it persists across multiple turns.
func (r *SessionLoopRegistry) GetOrCreate(
	ctx context.Context,
	sessionID string,
	createLoop func(sl *SessionLoop) (*adk.TurnLoop[TurnWorkItem, *schema.Message], context.CancelFunc),
) (*SessionLoop, error) {
	// Fast path
	r.mu.RLock()
	sl, ok := r.loops[sessionID]
	r.mu.RUnlock()
	if ok {
		return sl, nil
	}

	// Slow path
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check
	if sl, ok := r.loops[sessionID]; ok {
		return sl, nil
	}

	// Create SessionLoop first
	sl = &SessionLoop{
		sessionID:  sessionID,
		done:       make(chan struct{}),
		lastActive: time.Now(),
	}

	// Create TurnLoop using the SessionLoop (so OnAgentEvents references the same SessionLoop)
	loop, cancel := createLoop(sl)
	sl.loop = loop
	sl.cancel = cancel
	sl.recreateTurn = createLoop // Store for potential recreation

	// Start loop in background with an independent context.
	// This ensures the loop persists even after the caller's context is cancelled.
	// The loop will only stop when Stop() is called or when it has no more work.
	go func() {
		defer close(sl.done)
		// Use context.Background() so the loop is not tied to the caller's context
		loop.Run(context.Background())
		loop.Wait()
		if r.onStop != nil {
			r.onStop(sessionID)
		}
	}()

	r.loops[sessionID] = sl
	return sl, nil
}

// Get returns the loop for a session, or nil if not found.
func (r *SessionLoopRegistry) Get(sessionID string) *SessionLoop {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loops[sessionID]
}

// Remove removes a session loop from the registry.
func (r *SessionLoopRegistry) Remove(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.loops, sessionID)
}

// PushAndWait pushes a work item and waits for it to complete.
// If the loop has exited (e.g., due to interrupt), it will be recreated.
func (sl *SessionLoop) PushAndWait(ctx context.Context, item TurnWorkItem) error {
	// Check if loop needs recreation by checking if done channel is closed
	sl.mu.Lock()
	select {
	case <-sl.done:
		// Loop has exited, recreate it
		if sl.recreateTurn != nil {
			loop, cancel := sl.recreateTurn(sl)
			sl.loop = loop
			sl.cancel = cancel
			sl.done = make(chan struct{})

			// Start the new loop
			go func() {
				defer close(sl.done)
				loop.Run(context.Background())
				loop.Wait()
			}()
		}
	default:
		// Loop is still running
	}
	sl.mu.Unlock()

	errCh := make(chan error, 1)
	sl.mu.Lock()
	sl.waitCh = errCh
	sl.lastActive = time.Now()
	sl.mu.Unlock()

	// Add diagnostic logging
	pushed, err := sl.loop.Push(item)
	if !pushed {
		sl.mu.Lock()
		sl.waitCh = nil
		sl.mu.Unlock()
		// Log detailed diagnostic info
		return fmt.Errorf("failed to push to loop: loop may have stopped (session_id=%s, turn_id=%s, push_error=%v)",
			sl.sessionID, item.TurnID, err)
	}

	// Wait for turn completion.
	// Priority: errCh first (NotifyTurnDone is called before loop exits),
	// then done (loop exited without notifying), then ctx.
	select {
	case err := <-errCh:
		return err
	case <-sl.done:
		// Loop exited. Check if we have a result from NotifyTurnDone.
		// NotifyTurnDone is called before loop exits, so errCh should have the result.
		select {
		case err := <-errCh:
			return err
		default:
			// No result from NotifyTurnDone, loop exited unexpectedly.
			return fmt.Errorf("session loop exited unexpectedly during turn (session_id=%s, turn_id=%s)", sl.sessionID, item.TurnID)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NotifyTurnDone is called when a turn completes.
func (sl *SessionLoop) NotifyTurnDone(err error) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	if sl.waitCh != nil {
		sl.waitCh <- err
		close(sl.waitCh)
		sl.waitCh = nil
	}
}

// Stop stops the session loop gracefully.
func (sl *SessionLoop) Stop() {
	sl.loop.Stop()
	sl.cancel()
}

// Wait waits for the session loop to exit.
func (sl *SessionLoop) Wait() {
	<-sl.done
}
