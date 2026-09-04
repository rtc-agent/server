package rtcqueue_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// newTestQueue spins up a miniRedis server and returns a Queue bound to
// it plus a cleanup function.
func newTestQueue(t *testing.T) (*rtcqueue.Queue, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rtcqueue.New(rdb), mr
}

func TestPublishAndLoad(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	id, err := q.Publish(ctx, "s1", "hello", 5)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	w, err := q.LoadWork(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if w == nil {
		t.Fatal("work missing")
	}
	if w.SessionID != "s1" || w.Data != "hello" || w.Priority != 5 {
		t.Fatalf("unexpected work: %+v", w)
	}
	if w.Status != rtcqueue.StatusPending {
		t.Fatalf("status=%s, want pending", w.Status)
	}
}

func TestClaimPriorityOrder(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	// publish low-then-high priority
	q.Publish(ctx, "s1", "low", 1)
	q.Publish(ctx, "s1", "high", 100)

	res, err := q.Claim(ctx, "s1", "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res == nil {
		t.Fatal("expected claim success")
	}
	w, _ := q.LoadWork(ctx, res.WorkID)
	if w.Data != "high" {
		t.Fatalf("expected highest-priority work, got %q", w.Data)
	}
}

func TestSessionLock(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	q.Publish(ctx, "s1", "x", 1)

	// first claim wins
	r1, _ := q.Claim(ctx, "s1", "w1")
	if r1 == nil {
		t.Fatal("first claim should succeed")
	}
	// second claim loses (session locked)
	r2, _ := q.Claim(ctx, "s1", "w2")
	if r2 != nil {
		t.Fatal("second claim should return nil")
	}
}

func TestCompleteReleasesLock(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	id, _ := q.Publish(ctx, "s1", "x", 1)
	res, _ := q.Claim(ctx, "s1", "w1")
	if res == nil {
		t.Fatal("claim should succeed")
	}

	if err := q.Complete(ctx, res.WorkID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// lock should be released — a new worker can claim another work
	id2, _ := q.Publish(ctx, "s1", "y", 1)
	res2, _ := q.Claim(ctx, "s1", "w2")
	if res2 == nil {
		t.Fatal("claim after complete should succeed")
	}
	_ = id
	_ = id2
}

func TestCancelPending(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	id, _ := q.Publish(ctx, "s1", "x", 1)
	if err := q.Cancel(ctx, id, "nope"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	w, _ := q.LoadWork(ctx, id)
	if w.Status != rtcqueue.StatusCancelled {
		t.Fatalf("status=%s, want cancelled", w.Status)
	}
	// claiming the session should now fail (queue empty)
	res, _ := q.Claim(ctx, "s1", "w1")
	if res != nil {
		t.Fatal("expected nil claim after cancel")
	}
}

func TestCancelRefusesTerminal(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	id, _ := q.Publish(ctx, "s1", "x", 1)
	res, _ := q.Claim(ctx, "s1", "w1")
	q.Complete(ctx, res.WorkID)

	err := q.Cancel(ctx, id, "too late")
	if !errors.Is(err, rtcqueue.ErrAlreadyTerminal) {
		t.Fatalf("expected ErrAlreadyTerminal, got %v", err)
	}
}

func TestContentionOneWinner(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	q.Publish(ctx, "s1", "x", 1)

	const N = 10
	var (
		wg    sync.WaitGroup
		wins  atomic.Int64
		start = make(chan struct{})
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, _ := q.Claim(ctx, "s1", "w"+string(rune('A'+i)))
			if res != nil {
				wins.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 {
		t.Fatalf("expected 1 winner, got %d", wins.Load())
	}
}

func TestCrashRecovery(t *testing.T) {
	q, mr := newTestQueue(t)
	ctx := context.Background()

	q.Publish(ctx, "crash", "a", 10) // will be claimed then "crash"
	q.Publish(ctx, "crash", "b", 1)  // stays pending

	r1, _ := q.Claim(ctx, "crash", "crasher")
	if r1 == nil {
		t.Fatal("crasher should claim")
	}

	// new worker blocked while lock alive
	r2, _ := q.Claim(ctx, "crash", "rescuer")
	if r2 != nil {
		t.Fatal("rescuer should be blocked")
	}

	// simulate crash + TTL expiry
	mr.FastForward(121 * time.Second)

	r3, _ := q.Claim(ctx, "crash", "rescuer")
	if r3 == nil {
		t.Fatal("rescuer should claim after TTL")
	}
	w, _ := q.LoadWork(ctx, r3.WorkID)
	if w.Data != "b" {
		t.Fatalf("expected surviving work, got %q", w.Data)
	}

	// crashed work is stuck in processing
	cw, _ := q.LoadWork(ctx, r1.WorkID)
	if cw.Status != rtcqueue.StatusProcessing {
		t.Fatalf("crashed work status=%s, want processing", cw.Status)
	}
}

func TestRenewLockWrongOwner(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	q.Publish(ctx, "s1", "x", 1)
	q.Claim(ctx, "s1", "w1")

	ok, _ := q.RenewLock(ctx, "s1", "w2") // wrong owner
	if ok {
		t.Fatal("renew should refuse wrong owner")
	}
	ok, _ = q.RenewLock(ctx, "s1", "w1") // correct owner
	if !ok {
		t.Fatal("renew should succeed for owner")
	}
}

// TestRenewLockTOCTOU exercises the exact race the Lua rewrite was
// meant to prevent: worker A owns the lock, the lock expires, worker B
// re-acquires it, then worker A attempts a stale renewal. A's renewal
// must NOT extend B's lock.
func TestRenewLockTOCTOU(t *testing.T) {
	q, mr := newTestQueue(t)
	ctx := context.Background()

	q.Publish(ctx, "s1", "a", 10)
	q.Publish(ctx, "s1", "b", 1)

	// A claims (takes the lock), then the lock expires via FastForward.
	resA, _ := q.Claim(ctx, "s1", "A")
	if resA == nil {
		t.Fatal("A should claim")
	}
	mr.FastForward(121 * time.Second)

	// B claims — takes over the lock.
	resB, _ := q.Claim(ctx, "s1", "B")
	if resB == nil {
		t.Fatal("B should claim after TTL")
	}

	// A's stale renewal must fail (lock now owned by B).
	ok, err := q.RenewLock(ctx, "s1", "A")
	if err != nil {
		t.Fatalf("renew err: %v", err)
	}
	if ok {
		t.Fatal("stale renewal must return false, got true — A would have corrupted B's lock")
	}

	// B's renewal must still succeed.
	ok, _ = q.RenewLock(ctx, "s1", "B")
	if !ok {
		t.Fatal("B's renewal should succeed")
	}
}

func TestPublishNotifiesSessionNew(t *testing.T) {
	q, mr := newTestQueue(t)
	ctx := context.Background()

	sub := q.SubscribeNew(ctx)
	defer sub.Close()
	ch := sub.Channel()
	awaitSubscriberCount(t, mr, rtcqueue.ChannelSessionNew, 1, time.Now().Add(time.Second))

	id, err := q.Publish(ctx, "notify-sess", "hello", 5)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case msg := <-ch:
		if msg.Channel != rtcqueue.ChannelSessionNew {
			t.Fatalf("channel=%s, want %s", msg.Channel, rtcqueue.ChannelSessionNew)
		}
		if msg.Payload != "notify-sess" {
			t.Fatalf("payload=%q, want session id", msg.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session:new notification")
	}
	_ = id
}

func TestCancelNotifiesSessionCancel(t *testing.T) {
	q, mr := newTestQueue(t)
	ctx := context.Background()

	// subscribe BEFORE publishing + claiming so the cancel message is
	// not missed.
	cancelCh := rtcqueue.ChannelSessionCancel("cancel-sess")
	sub := q.SubscribeCancel(ctx, "cancel-sess")
	defer sub.Close()
	ch := sub.Channel()
	awaitSubscriberCount(t, mr, cancelCh, 1, time.Now().Add(time.Second))

	id, _ := q.Publish(ctx, "cancel-sess", "will-cancel", 5)
	q.Claim(ctx, "cancel-sess", "w1") // move to processing

	if err := q.Cancel(ctx, id, "because"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	select {
	case msg := <-ch:
		var cm rtcqueue.CancelMessage
		if err := json.Unmarshal([]byte(msg.Payload), &cm); err != nil {
			t.Fatalf("unmarshal cancel message: %v", err)
		}
		if cm.WorkID != id {
			t.Fatalf("work_id=%s, want %s", cm.WorkID, id)
		}
		if cm.Reason != "because" {
			t.Fatalf("reason=%q, want %q", cm.Reason, "because")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancel notification")
	}
}

func TestLoadWorkParseError(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	// bypass Publish and write a corrupt hash directly
	id := "corrupt-work"
	q.Client().HSet(ctx, "work:"+id, map[string]interface{}{
		"id": id, "session_id": "s", "data": "d",
		"priority": "not-a-number", "status": "pending",
		"created_at": "1", "updated_at": "2",
	})

	_, err := q.LoadWork(ctx, id)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestLoadWorkMissing(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	w, err := q.LoadWork(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if w != nil {
		t.Fatalf("expected nil work, got %+v", w)
	}
}

// awaitSubscriberCount polls miniRedis's PubSubNumSub until the given
// channel has at least `want` subscribers, or the deadline elapses.
// This replaces a fixed sleep and is robust under heavy CI load.
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
