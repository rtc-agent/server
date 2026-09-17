package lifecycle_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rtc-agent/server/internal/lifecycle"
)

// mockComponent implements lifecycle.Component for testing.
type mockComponent struct {
	startFn    func(ctx context.Context) error
	stopFn     func(ctx context.Context) error
	healthFn   func(ctx context.Context) error
	startCalls atomic.Int32
	stopCalls  atomic.Int32
}

func newMockComponent() *mockComponent {
	return &mockComponent{}
}

func (m *mockComponent) Start(ctx context.Context) error {
	m.startCalls.Add(1)
	if m.startFn != nil {
		return m.startFn(ctx)
	}
	return nil
}

func (m *mockComponent) Stop(ctx context.Context) error {
	m.stopCalls.Add(1)
	if m.stopFn != nil {
		return m.stopFn(ctx)
	}
	return nil
}

func (m *mockComponent) HealthCheck(ctx context.Context) error {
	if m.healthFn != nil {
		return m.healthFn(ctx)
	}
	return nil
}

func TestManagerStartStop(t *testing.T) {
	m := lifecycle.NewManager()
	c1 := newMockComponent()
	c2 := newMockComponent()

	m.Register("c1", c1)
	m.Register("c2", c2)

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Both components started.
	if got := c1.startCalls.Load(); got != 1 {
		t.Errorf("c1 start calls = %d, want 1", got)
	}
	if got := c2.startCalls.Load(); got != 1 {
		t.Errorf("c2 start calls = %d, want 1", got)
	}

	// Stop should stop components in reverse order.
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := c1.stopCalls.Load(); got != 1 {
		t.Errorf("c1 stop calls = %d, want 1", got)
	}
	if got := c2.stopCalls.Load(); got != 1 {
		t.Errorf("c2 stop calls = %d, want 1", got)
	}
}

func TestManagerStartTwiceFails(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := m.Start(ctx); err == nil {
		t.Fatal("second Start should return error")
	}
}

func TestManagerStopIdempotent(t *testing.T) {
	m := lifecycle.NewManager()
	c := newMockComponent()
	m.Register("c", c)

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Stop twice — component should only be stopped once.
	m.Stop(ctx)
	m.Stop(ctx)

	if got := c.stopCalls.Load(); got != 1 {
		t.Errorf("stop calls = %d, want 1 (idempotent)", got)
	}
}

func TestManagerStopBeforeStart(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	// Stop before Start should be a no-op (not panic, not error).
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

func TestManagerStartRollback(t *testing.T) {
	m := lifecycle.NewManager()
	c1 := newMockComponent()
	c2 := newMockComponent()
	c3 := newMockComponent()

	// c2 fails to start.
	c2.startFn = func(ctx context.Context) error {
		return errors.New("start failed")
	}

	m.Register("c1", c1)
	m.Register("c2", c2)
	m.Register("c3", c3)

	ctx := context.Background()
	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start should return error when c2 fails")
	}

	// c1 was started before c2, so it should be stopped (rollback).
	if got := c1.stopCalls.Load(); got != 1 {
		t.Errorf("c1 stop calls (rollback) = %d, want 1", got)
	}
	// c3 was never started.
	if got := c3.startCalls.Load(); got != 0 {
		t.Errorf("c3 start calls = %d, want 0 (not started)", got)
	}
}

func TestManagerStopContinuesOnError(t *testing.T) {
	m := lifecycle.NewManager()
	c1 := newMockComponent()
	c2 := newMockComponent()

	// c1 fails to stop.
	c1.stopFn = func(ctx context.Context) error {
		return errors.New("stop failed")
	}

	m.Register("c1", c1)
	m.Register("c2", c2)

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Even though c1 fails, c2 should still be stopped.
	m.Stop(ctx)

	if got := c2.stopCalls.Load(); got != 1 {
		t.Errorf("c2 stop calls = %d, want 1 (other components still stopped)", got)
	}
}

func TestManagerHealthCheck(t *testing.T) {
	m := lifecycle.NewManager()
	c1 := newMockComponent()
	c2 := newMockComponent()

	c2.healthFn = func(ctx context.Context) error {
		return errors.New("unhealthy")
	}

	m.Register("c1", c1)
	m.Register("c2", c2)

	ctx := context.Background()
	err := m.HealthCheck(ctx)
	if err == nil {
		t.Fatal("HealthCheck should return error for unhealthy component")
	}
}

func TestManagerHealthCheckAllHealthy(t *testing.T) {
	m := lifecycle.NewManager()
	c1 := newMockComponent()
	c2 := newMockComponent()

	m.Register("c1", c1)
	m.Register("c2", c2)

	ctx := context.Background()
	if err := m.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}

func TestManagerGo(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	// Start with no components — Go should still work.
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var started atomic.Bool
	var finished atomic.Bool

	m.Go("test-goroutine", func(ctx context.Context) {
		started.Store(true)
		<-ctx.Done()
		finished.Store(true)
	})

	// Wait for goroutine to start.
	deadline := time.After(2 * time.Second)
	for !started.Load() {
		select {
		case <-deadline:
			t.Fatal("goroutine did not start within 2s")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Stop should cancel context and wait for goroutine.
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !finished.Load() {
		t.Error("goroutine did not finish after Stop")
	}
}

func TestManagerGoPanicRecovery(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	m.Go("panicking", func(ctx context.Context) {
		defer close(done)
		panic("test panic")
	})

	// Wait for goroutine to finish (panic should be recovered).
	select {
	case <-done:
		// OK — panic was recovered.
	case <-time.After(2 * time.Second):
		t.Fatal("panicking goroutine did not finish within 2s")
	}

	// Stop should still work even after panic.
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after panic: %v", err)
	}
}

func TestManagerStopTimeout(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Launch a goroutine that ignores context cancellation.
	m.Go("stubborn", func(ctx context.Context) {
		// Block forever, ignoring ctx.Done().
		select {}
	})

	// Stop with a short timeout should return context error.
	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := m.Stop(stopCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop timeout error = %v, want DeadlineExceeded", err)
	}
}

func TestManagerConcurrentStop(t *testing.T) {
	m := lifecycle.NewManager()
	c := newMockComponent()
	m.Register("c", c)

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Launch multiple concurrent Stop calls.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Stop(context.Background())
		}()
	}
	wg.Wait()

	// Component should only be stopped once (sync.Once).
	if got := c.stopCalls.Load(); got != 1 {
		t.Errorf("stop calls = %d, want 1 (concurrent Stop dedup)", got)
	}
}

func TestManagerShutdownCh(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	ch := m.ShutdownCh()

	// Before Stop, channel should be open.
	select {
	case <-ch:
		t.Fatal("ShutdownCh closed before Stop")
	default:
		// OK
	}

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Still open.
	select {
	case <-ch:
		t.Fatal("ShutdownCh closed before Stop (after Start)")
	default:
	}

	m.Stop(ctx)

	// After Stop, channel should be closed.
	select {
	case <-ch:
		// OK
	case <-time.After(time.Second):
		t.Fatal("ShutdownCh not closed after Stop")
	}
}

func TestManagerRegisterAfterStartPanics(t *testing.T) {
	m := lifecycle.NewManager()
	ctx := context.Background()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register after Start should panic")
		}
	}()

	m.Register("late", newMockComponent())
}
