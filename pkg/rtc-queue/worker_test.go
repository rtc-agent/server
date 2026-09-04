package rtcqueue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

func newTestWorker(t *testing.T, onWork func(context.Context, *rtcqueue.Work, <-chan rtcqueue.CancelMessage) error) (*rtcqueue.Worker, *rtcqueue.Queue, *miniredis.Miniredis) {
	t.Helper()
	q, mr := newTestQueue(t)
	cfg := rtcqueue.WorkerConfig{
		WorkerID: "test-worker",
		OnWork:   onWork,
	}
	w := rtcqueue.NewWorker(q, cfg)
	return w, q, mr
}

func TestWorkerBasic(t *testing.T) {
	w, q, _ := newTestWorker(t, func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// wait for subscription
	time.Sleep(50 * time.Millisecond)

	id, _ := q.Publish(ctx, "s1", "hello", 5)

	// wait for processing
	time.Sleep(100 * time.Millisecond)

	work, _ := q.LoadWork(context.Background(), id)
	if work.Status != rtcqueue.StatusCompleted {
		t.Fatalf("expected completed, got %s", work.Status)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err: %v", err)
	}
}

func TestWorkerCancelDuringProcessing(t *testing.T) {
	started := make(chan struct{})
	w, q, _ := newTestWorker(t, func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
		close(started)
		select {
		case cm := <-cancel:
			if cm.Reason != "admin-stop" {
				t.Errorf("reason=%q, want admin-stop", cm.Reason)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	id, _ := q.Publish(ctx, "s1", "will-cancel", 5)

	<-started // wait for OnWork to start
	time.Sleep(50 * time.Millisecond)

	if err := q.Cancel(context.Background(), id, "admin-stop"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	work, _ := q.LoadWork(context.Background(), id)
	if work.Status != rtcqueue.StatusCancelled {
		t.Fatalf("expected cancelled, got %s", work.Status)
	}

	cancel()
}

func TestWorkerConcurrencyLimit(t *testing.T) {
	var active atomic.Int64
	var maxActive atomic.Int64

	q, _ := newTestQueue(t)
	w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
		WorkerID:    "concurrent-worker",
		Concurrency: 2,
		OnWork: func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
			n := active.Add(1)
			for {
				old := maxActive.Load()
				if n <= old || maxActive.CompareAndSwap(old, n) {
					break
				}
			}
			defer active.Add(-1)
			time.Sleep(100 * time.Millisecond)
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// publish to 5 different sessions — only 2 should be processed at once
	for i := 0; i < 5; i++ {
		q.Publish(ctx, "sess-"+string(rune('a'+i)), "data", 1)
	}

	time.Sleep(500 * time.Millisecond)
	if m := maxActive.Load(); m > 2 {
		t.Fatalf("max concurrency was %d, want <= 2", m)
	}
	cancel()
}

func TestWorkerProcessesMultipleSessions(t *testing.T) {
	var processed []string
	var mu sync.Mutex

	w, q, _ := newTestWorker(t, func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
		mu.Lock()
		processed = append(processed, work.Data)
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// publish to 3 different sessions — each triggers a fresh claim
	q.Publish(ctx, "sess-a", "a", 1)
	time.Sleep(20 * time.Millisecond)
	q.Publish(ctx, "sess-b", "b", 2)
	time.Sleep(20 * time.Millisecond)
	q.Publish(ctx, "sess-c", "c", 3)

	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 3 {
		t.Fatalf("processed %d works, want 3: %v", len(processed), processed)
	}
	cancel()
}

func TestWorkerLockLost(t *testing.T) {
	onWorkCalled := make(chan struct{})
	workFinished := make(chan error, 1)

	q, mr := newTestQueue(t)
	w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
		WorkerID:      "test-worker",
		RenewInterval: 50 * time.Millisecond, // short for fast test
		OnWork: func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
			close(onWorkCalled)
			// block until ctx cancelled (simulating long-running work)
			<-ctx.Done()
			workFinished <- ctx.Err()
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// publish 2 works to the same session; our worker claims the first
	q.Publish(ctx, "lock-lost-sess", "first", 10)
	q.Publish(ctx, "lock-lost-sess", "second", 1)
	<-onWorkCalled

	// expire the lock
	mr.FastForward(121 * time.Second)

	// another process claims the session — gets the SECOND work (first
	// was already popped by our worker)
	res, _ := q.Claim(context.Background(), "lock-lost-sess", "other-worker")
	if res == nil {
		t.Fatal("other-worker should claim after TTL")
	}

	// wait for our worker's OnWork to be aborted (it detects lock loss
	// on the next renewal tick)
	select {
	case err := <-workFinished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnWork was not aborted after lock lost")
	}

	cancel()
}

func TestWorkerGracefulShutdown(t *testing.T) {
	workStarted := make(chan struct{})
	workDone := make(chan struct{})

	w, q, _ := newTestWorker(t, func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
		close(workStarted)
		time.Sleep(200 * time.Millisecond) // simulate real work
		close(workDone)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	q.Publish(ctx, "shutdown-sess", "data", 5)

	<-workStarted

	// stop while work is in progress — should wait for completion
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if err := w.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// work should have completed before stop returned
	select {
	case <-workDone:
		// good
	default:
		t.Fatal("work was not completed before Stop returned")
	}
}

func TestWorkerCallbackError(t *testing.T) {
	var errCalled atomic.Bool
	q, _ := newTestQueue(t)
	w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
		WorkerID: "err-worker",
		OnWork: func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
			return errors.New("simulated failure")
		},
		OnError: func(err error) {
			errCalled.Store(true)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	id, _ := q.Publish(ctx, "err-sess", "data", 5)

	time.Sleep(100 * time.Millisecond)

	if !errCalled.Load() {
		t.Fatal("OnError was not called")
	}

	// work should remain in processing state (not completed, not cancelled)
	work, _ := q.LoadWork(context.Background(), id)
	if work.Status != rtcqueue.StatusProcessing {
		t.Fatalf("status=%s, want processing (left for manual recovery)", work.Status)
	}

	cancel()
}
