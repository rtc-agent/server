// Example: demonstrates the rtc-queue primitives against a miniRedis
// backend. Two phases:
//
//	Phase 1 — pub/sub driven workers consume works across two sessions.
//	Phase 2 — multi-worker contention: three workers race to claim the
//	           same session; only one wins.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

func main() {
	mr, err := miniredis.Run()
	if err != nil {
		log.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	fmt.Printf("miniRedis up at %s\n\n", mr.Addr())

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	q := rtcqueue.New(rdb)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 3*time.Second)
	phasePubSub(ctx1, cancel1, q)
	cancel1()

	fmt.Println("\n--- phase 2: multi-worker contention ---")
	ctx2 := context.Background()
	phaseContention(ctx2, q)

	fmt.Println("\n--- phase 3: worker crash recovery ---")
	phaseCrash(ctx2, mr, q)

	fmt.Println("\n--- phase 4: Worker API (callback-driven lifecycle) ---")
	phaseWorkerAPI(context.Background(), q)

	fmt.Println("\nbye")
}

// -----------------------------------------------------------------------
// Phase 1: pub/sub driven workers consume works across two sessions.
// -----------------------------------------------------------------------
func phasePubSub(ctx context.Context, cancel context.CancelFunc, q *rtcqueue.Queue) {
	fmt.Println("--- phase 1: pub/sub driven workers ---")

	// subscribe workers BEFORE publishing
	var wg sync.WaitGroup
	runWorker := func(workerID string) {
		defer wg.Done()
		sub := q.SubscribeNew(ctx)
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				claim, err := q.Claim(ctx, msg.Payload, workerID)
				if err != nil || claim == nil {
					continue
				}
				processAndDrain(ctx, q, workerID, claim)
			}
		}
	}
	wg.Add(2)
	go runWorker("worker-A")
	go runWorker("worker-B")
	time.Sleep(50 * time.Millisecond)

	publish := func(sessionID, data string, prio int64) string {
		id, err := q.Publish(ctx, sessionID, data, prio)
		if err != nil {
			log.Fatalf("publish: %v", err)
		}
		fmt.Printf("[producer] published %s (session=%s prio=%d data=%q)\n",
			id[:8], sessionID, prio, data)
		return id
	}

	s1w1 := publish("session-1", "hello-1", 1)
	_ = publish("session-1", "hello-2", 10)
	_ = publish("session-2", "world-1", 5)

	time.Sleep(400 * time.Millisecond)

	// demonstrate cancel refusal on completed work
	err := q.Cancel(ctx, s1w1, "too late")
	fmt.Printf("[cancel] work %s: err=%v\n", s1w1[:8], err)

	cancel() // wind down phase 1 workers
	wg.Wait()
}

// -----------------------------------------------------------------------
// Phase 2: multi-worker contention on the SAME session.
// -----------------------------------------------------------------------
func phaseContention(ctx context.Context, q *rtcqueue.Queue) {
	// publish three works on the same session
	var ids []string
	for i, prio := range []int64{1, 5, 10} {
		id, _ := q.Publish(ctx, "hot-session", fmt.Sprintf("task-%d", i), prio)
		ids = append(ids, id)
		fmt.Printf("[producer] published %s (prio=%d)\n", id[:8], prio)
	}

	// three workers race to claim the same session
	const N = 3
	var (
		wg     sync.WaitGroup
		wins   atomic.Int64
		losses atomic.Int64
		start  = make(chan struct{})
	)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			workerID := fmt.Sprintf("racer-%d", id)
			<-start // synchronize the start
			res, err := q.Claim(ctx, "hot-session", workerID)
			if err != nil {
				log.Printf("[%s] claim err: %v", workerID, err)
				return
			}
			if res == nil {
				losses.Add(1)
				fmt.Printf("[%s] lost the race (session locked)\n", workerID)
				return
			}
			wins.Add(1)
			work, _ := q.LoadWork(ctx, res.WorkID)
			fmt.Printf("[%s] WON — claimed work %s (data=%q prio=%d)\n",
				workerID, res.WorkID[:8], work.Data, work.Priority)
		}(i)
	}
	close(start) // all goroutines fire at once
	wg.Wait()

	fmt.Printf("\nresult: %d winner(s), %d loser(s)\n", wins.Load(), losses.Load())
	if wins.Load() != 1 {
		fmt.Println("ERROR: expected exactly one winner")
	}

	// second wave: release and let another worker take over
	fmt.Println("\n[phase 2b] release session, second wave")
	_ = q.ReleaseSession(ctx, "hot-session")

	w, _ := q.Claim(ctx, "hot-session", "racer-late")
	if w != nil {
		work, _ := q.LoadWork(ctx, w.WorkID)
		fmt.Printf("[racer-late] claimed work %s (data=%q prio=%d)\n",
			w.WorkID[:8], work.Data, work.Priority)
	}

	// cancel the remaining pending work
	for _, id := range ids {
		if err := q.Cancel(ctx, id, "cleanup"); err != nil && !errors.Is(err, rtcqueue.ErrAlreadyTerminal) {
			// ignore already-terminal errors
		}
	}
}

// -----------------------------------------------------------------------
// Phase 3: worker crash — worker claims a work (and takes the session
// lock) then dies without releasing. After the lock TTL elapses (fast-
// forwarded via miniRedis), another worker can claim the same session.
// -----------------------------------------------------------------------
func phaseCrash(ctx context.Context, mr *miniredis.Miniredis, q *rtcqueue.Queue) {
	// publish two works; the first will be claimed, then the worker
	// "crashes" — the second stays pending in the queue.
	id1, _ := q.Publish(ctx, "crash-session", "will-crash", 10)
	id2, _ := q.Publish(ctx, "crash-session", "will-survive", 1)
	fmt.Printf("[producer] published %s (prio=10) and %s (prio=1)\n", id1[:8], id2[:8])

	// worker-1 claims the highest-priority work, takes the lock, then dies
	res, _ := q.Claim(ctx, "crash-session", "worker-crash")
	if res != nil {
		fmt.Printf("[worker-crash] claimed %s — now simulating crash (no release)\n", res.WorkID[:8])
	}

	// sanity: another worker should fail to claim right now
	res2, _ := q.Claim(ctx, "crash-session", "worker-rescue")
	if res2 == nil {
		fmt.Println("[worker-rescue] blocked (session lock held by crashed worker)")
	} else {
		fmt.Printf("[worker-rescue] ERROR: claimed unexpectedly (work=%s)\n", res2.WorkID[:8])
	}

	// fast-forward miniRedis past the lock TTL
	fmt.Println("[time travel] advancing 121s past lock TTL...")
	mr.FastForward(121 * time.Second)

	// now worker-rescue should be able to claim the remaining work
	res3, _ := q.Claim(ctx, "crash-session", "worker-rescue")
	if res3 != nil {
		work, _ := q.LoadWork(ctx, res3.WorkID)
		fmt.Printf("[worker-rescue] recovered — claimed work %s (data=%q)\n",
			res3.WorkID[:8], work.Data)
	} else {
		fmt.Println("[worker-rescue] queue empty (unexpected)")
	}

	// inspect final state of the crashed worker's work
	w, _ := q.LoadWork(ctx, id1)
	fmt.Printf("[post-crash] work %s is stuck in status=%s (no requeue)\n",
		id1[:8], w.Status)
}

// -----------------------------------------------------------------------
// Phase 4: Worker API — callback-driven lifecycle.
// The Worker handles: subscribe → claim → renew → cancel listen → complete
// → graceful shutdown. The user only provides an OnWork callback.
// -----------------------------------------------------------------------
func phaseWorkerAPI(ctx context.Context, q *rtcqueue.Queue) {
	ctx, cancel := context.WithCancel(ctx)

	w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
		WorkerID:      "callback-worker",
		Concurrency:   2,
		RenewInterval: 100 * time.Millisecond, // short for demo
		OnWork: func(ctx context.Context, work *rtcqueue.Work, cancelCh <-chan rtcqueue.CancelMessage) error {
			fmt.Printf("[OnWork] start %s (data=%q prio=%d)\n",
				work.ID[:8], work.Data, work.Priority)

			// simulate work; watch for cancel
			select {
			case cm := <-cancelCh:
				fmt.Printf("[OnWork] cancelled: %s\n", cm.Reason)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
				fmt.Printf("[OnWork] done %s\n", work.ID[:8])
				return nil
			}
		},
		OnError: func(err error) {
			fmt.Printf("[OnError] %v\n", err)
		},
	})

	// run worker in background
	go func() {
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("worker err: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond) // let subscription register

	// publish 3 works to different sessions
	id1, _ := q.Publish(ctx, "worker-sess-1", "task-A", 5)
	id2, _ := q.Publish(ctx, "worker-sess-2", "task-B", 10)
	id3, _ := q.Publish(ctx, "worker-sess-3", "task-C", 1)
	_ = id1
	_ = id2

	time.Sleep(300 * time.Millisecond)

	// cancel the third one mid-processing (or before)
	_ = q.Cancel(ctx, id3, "demo")

	time.Sleep(100 * time.Millisecond)

	// graceful shutdown
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := w.Stop(stopCtx); err != nil {
		log.Printf("stop: %v", err)
	}
	fmt.Println("[phase 4] worker stopped gracefully")
	cancel()
}

func processAndDrain(ctx context.Context, q *rtcqueue.Queue, workerID string, claim *rtcqueue.ClaimResult) {
	work, _ := q.LoadWork(ctx, claim.WorkID)
	if work == nil {
		return
	}
	fmt.Printf("[%s] claimed work %s (session=%s data=%q prio=%d)\n",
		workerID, claim.WorkID[:8], claim.SessionID, work.Data, work.Priority)

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				q.RenewLock(ctx, claim.SessionID, workerID)
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(done)

	if err := q.Complete(ctx, claim.WorkID); err != nil {
		log.Printf("[%s] complete: %v", workerID, err)
		return
	}
	fmt.Printf("[%s] completed work %s\n", workerID, claim.WorkID[:8])

	for {
		next, err := q.Claim(ctx, claim.SessionID, workerID)
		if err != nil || next == nil {
			return
		}
		nw, _ := q.LoadWork(ctx, next.WorkID)
		if nw == nil {
			return
		}
		fmt.Printf("[%s] drained work %s (data=%q)\n", workerID, next.WorkID[:8], nw.Data)
		_ = q.Complete(ctx, next.WorkID)
	}
}
