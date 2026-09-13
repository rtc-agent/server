package agent

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThrottle_ImmediateExecution(t *testing.T) {
	throttle := NewThrottle(100 * time.Millisecond)
	var called atomic.Int32

	throttle.Do("key1", func() {
		called.Add(1)
	})

	if called.Load() != 1 {
		t.Errorf("expected immediate execution, called = %d, want 1", called.Load())
	}
}

func TestThrottle_ThrottledExecution(t *testing.T) {
	throttle := NewThrottle(100 * time.Millisecond)
	var called atomic.Int32

	// First call: immediate
	throttle.Do("key1", func() { called.Add(1) })
	if called.Load() != 1 {
		t.Fatalf("first call: called = %d, want 1", called.Load())
	}

	// Second call within interval: throttled
	throttle.Do("key1", func() { called.Add(1) })
	if called.Load() != 1 {
		t.Fatalf("second call (throttled): called = %d, want 1", called.Load())
	}

	// Wait for timer to fire
	time.Sleep(150 * time.Millisecond)
	if called.Load() != 2 {
		t.Errorf("after timer: called = %d, want 2", called.Load())
	}
}

func TestThrottle_LastCallWins(t *testing.T) {
	throttle := NewThrottle(100 * time.Millisecond)
	var result atomic.Value
	result.Store("")

	// First call: immediate
	throttle.Do("key1", func() { result.Store("first") })

	// Multiple throttled calls: only the last should execute
	throttle.Do("key1", func() { result.Store("second") })
	throttle.Do("key1", func() { result.Store("third") })
	throttle.Do("key1", func() { result.Store("fourth") })

	// Wait for timer
	time.Sleep(150 * time.Millisecond)

	if got := result.Load().(string); got != "fourth" {
		t.Errorf("expected last throttled call to win, got %q, want %q", got, "fourth")
	}
}

func TestThrottle_IndependentKeys(t *testing.T) {
	throttle := NewThrottle(100 * time.Millisecond)
	var countA, countB atomic.Int32

	// key A: immediate
	throttle.Do("A", func() { countA.Add(1) })
	// key B: immediate (independent of A)
	throttle.Do("B", func() { countB.Add(1) })

	if countA.Load() != 1 || countB.Load() != 1 {
		t.Fatalf("independent keys: A=%d B=%d, want 1, 1", countA.Load(), countB.Load())
	}

	// Throttle both
	throttle.Do("A", func() { countA.Add(1) })
	throttle.Do("B", func() { countB.Add(1) })

	// Both should still be 1 (throttled)
	if countA.Load() != 1 || countB.Load() != 1 {
		t.Fatalf("throttled: A=%d B=%d, want 1, 1", countA.Load(), countB.Load())
	}

	time.Sleep(150 * time.Millisecond)

	// Both should have executed their pending fn
	if countA.Load() != 2 || countB.Load() != 2 {
		t.Errorf("after timer: A=%d B=%d, want 2, 2", countA.Load(), countB.Load())
	}
}

func TestThrottle_ConcurrentSafety(t *testing.T) {
	throttle := NewThrottle(50 * time.Millisecond)
	var count atomic.Int32
	var wg sync.WaitGroup

	// Launch 100 concurrent calls on the same key
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			throttle.Do("concurrent", func() { count.Add(1) })
		}()
	}

	wg.Wait()

	// At least 1 call should have executed immediately
	if count.Load() < 1 {
		t.Errorf("concurrent: count = %d, want >= 1", count.Load())
	}

	// Wait for all timers to settle
	time.Sleep(200 * time.Millisecond)

	// Should not have excessive executions (first + at most a few timer firings)
	if count.Load() > 10 {
		t.Errorf("concurrent: count = %d, seems too high (possible race)", count.Load())
	}
}

func TestThrottle_NoPendingAfterExecution(t *testing.T) {
	throttle := NewThrottle(50 * time.Millisecond)
	var count atomic.Int32

	// Immediate
	throttle.Do("k", func() { count.Add(1) })
	// Throttled
	throttle.Do("k", func() { count.Add(1) })

	time.Sleep(100 * time.Millisecond)
	// Timer fired, pending executed
	if count.Load() != 2 {
		t.Fatalf("after first cycle: count = %d, want 2", count.Load())
	}

	// Now call again — should be immediate (interval has passed)
	throttle.Do("k", func() { count.Add(1) })
	if count.Load() != 3 {
		t.Errorf("after third call: count = %d, want 3 (immediate execution)", count.Load())
	}
}
