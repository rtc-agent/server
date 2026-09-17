package agent

import (
	"sync"
	"time"
)

// Throttle ensures that operations for the same key are spaced at least minInterval apart.
//
// Calls within the interval are deferred until the interval ends (only the last call is kept).
// Uses time.AfterFunc instead of goroutine + sleep to avoid goroutine leaks.
//
// Each key is throttled independently, with no interference between keys.
type Throttle struct {
	minInterval time.Duration
	mu          sync.Mutex
	lastCall    map[string]time.Time
	pendingFn   map[string]func()
	timers      map[string]*time.Timer
}

// NewThrottle creates a new Throttle.
func NewThrottle(minInterval time.Duration) *Throttle {
	return &Throttle{
		minInterval: minInterval,
		lastCall:    make(map[string]time.Time),
		pendingFn:   make(map[string]func()),
		timers:      make(map[string]*time.Timer),
	}
}

// Do executes fn with throttle control.
//
//   - If elapsed time since last execution >= minInterval (or first call): execute fn immediately
//   - If within the interval: record fn as pending (overwriting any previous), wait for timer to fire
//
// Multiple Do calls for the same key within one interval will only execute the last fn.
// Different keys are independent and do not affect each other.
func (t *Throttle) Do(key string, fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	if last, ok := t.lastCall[key]; ok && now.Sub(last) < t.minInterval {
		// Within throttle interval: record pending function (overwriting previous), wait for timer.
		t.pendingFn[key] = fn
		return
	}

	// Execute immediately.
	t.lastCall[key] = now
	fn()

	// If a timer is already running, no need to create another (it will check pendingFn when it fires).
	if _, exists := t.timers[key]; exists {
		return
	}

	// Create a timer to check and execute the pending function after the interval.
	// NOTE: The timer callback runs in its own goroutine (created by time.AfterFunc).
	// It acquires t.mu independently, so if Stop() is called while the callback is
	// waiting for the lock, Stop will cancel the timer but the callback will still
	// execute. This is safe because the callback checks pendingFn (which Stop clears)
	// and releases the lock promptly.
	last := t.lastCall[key]
	t.timers[key] = time.AfterFunc(t.minInterval, func() {
		t.mu.Lock()
		delete(t.timers, key)
		// Set lastCall to the theoretical interval end point (not time.Now()),
		// to prevent timer delay from making subsequent time-difference calculations too small.
		t.lastCall[key] = last.Add(t.minInterval)
		if pendingFn, ok := t.pendingFn[key]; ok {
			delete(t.pendingFn, key)
			t.mu.Unlock()
			pendingFn()
		} else {
			t.mu.Unlock()
		}
	})
}

// Stop cancels all pending timers and releases references to pending functions.
// This should be called when the owning agent/session is being shut down to
// prevent timer leaks and allow GC of captured closures.
//
// After Stop, the Throttle must not be used (Do calls may panic or behave
// unpredictably).
func (t *Throttle) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Cancel all pending timers to prevent them from firing after shutdown.
	for key, timer := range t.timers {
		timer.Stop()
		delete(t.timers, key)
	}

	// Clear pending functions to release references held by closures,
	// allowing the GC to reclaim captured objects (e.g. agent, session).
	for key := range t.pendingFn {
		delete(t.pendingFn, key)
	}

	// Clear lastCall to fully reset state.
	for key := range t.lastCall {
		delete(t.lastCall, key)
	}
}
