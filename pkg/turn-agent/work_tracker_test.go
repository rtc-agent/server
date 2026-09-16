package turnagent

import (
	"testing"
	"time"
)

// These tests cover WorkTracker edge cases not exercised in turn_agent_test.go.

func TestWorkTracker_IsAbandoned_ChannelStillOpen(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()
	tracker.Register("work-1")

	// Before the channel is closed (neither Complete nor CompleteAll),
	// IsAbandoned returns true because the entry exists in the map.
	// This is the "still pending" state — not truly abandoned yet.
	// The caller should only call IsAbandoned after the channel closes.
	if !tracker.IsAbandoned("work-1") {
		t.Fatal("before any close, entry is still in pending map")
	}
}

func TestWorkTracker_IsAbandoned_AfterComplete(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()
	tracker.Register("work-1")
	tracker.Complete("work-1")

	// Normal Complete removes the entry, so it's not abandoned.
	if tracker.IsAbandoned("work-1") {
		t.Fatal("should not be abandoned after normal Complete")
	}
}

func TestWorkTracker_IsAbandoned_AfterCompleteAll(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()
	tracker.Register("work-1")
	tracker.Register("work-2")
	tracker.CompleteAll()

	// CompleteAll closes channels but leaves entries — they are abandoned.
	if !tracker.IsAbandoned("work-1") {
		t.Fatal("work-1 should be abandoned after CompleteAll")
	}
	if !tracker.IsAbandoned("work-2") {
		t.Fatal("work-2 should be abandoned after CompleteAll")
	}
}

func TestWorkTracker_IsAbandoned_UnknownID(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	if tracker.IsAbandoned("nonexistent") {
		t.Fatal("unknown workID should not be abandoned")
	}
}

func TestWorkTracker_RegisterDuplicateReturnsExistingChannel(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()

	ch1 := tracker.Register("work-1")
	ch2 := tracker.Register("work-1")

	if ch1 != ch2 {
		t.Fatal("duplicate Register should return the same channel")
	}
}

func TestWorkTracker_RegisterAfterCompleteAll(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()
	tracker.CompleteAll()

	// Register after CompleteAll should return a pre-closed channel.
	ch := tracker.Register("work-late")
	select {
	case <-ch:
		// OK — pre-closed channel
	case <-time.After(time.Second):
		t.Fatal("Register after CompleteAll should return a closed channel")
	}

	// The late-registered item is NOT considered abandoned since
	// CompleteAll happened before it was registered.
	if tracker.IsAbandoned("work-late") {
		t.Fatal("late-registered work should not be abandoned")
	}
}

func TestWorkTracker_CompleteAfterCompleteAll(t *testing.T) {
	t.Parallel()
	tracker := NewWorkTracker()
	tracker.Register("work-1")

	tracker.CompleteAll()
	// Complete after CompleteAll should not panic (channel already closed).
	tracker.Complete("work-1")

	// After Complete removes the entry, IsAbandoned returns false.
	if tracker.IsAbandoned("work-1") {
		t.Fatal("after Complete, entry should be removed from pending")
	}
}
