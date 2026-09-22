package rtcqueue_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// TestMultiWorkerContention verifies that multiple workers competing for the
// same session in HoldLock mode do not process work concurrently. This test
// validates the fix for the distributed session race condition where
// ReleaseSession without credential check could delete another worker's lock.
func TestMultiWorkerContention(t *testing.T) {
	const (
		numWorkers    = 3
		numWorkItems  = 10
		sessionID     = "contention-test-session"
		workerTimeout = 5 * time.Second
	)

	q, mr := newTestQueue(t)
	defer mr.Close()

	// Track concurrent processing
	var activeWorkers atomic.Int64
	var maxConcurrent atomic.Int64
	var processedCount atomic.Int64
	var mu sync.Mutex
	processingWorkers := make(map[string]bool)

	// Create multiple workers competing for the same session
	workers := make([]*rtcqueue.Worker, numWorkers)
	for i := 0; i < numWorkers; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		w := rtcqueue.NewWorker(q, rtcqueue.WorkerConfig{
			WorkerID:    workerID,
			Concurrency: 1,
			HoldLock:    true,
			RenewInterval: 100 * time.Millisecond, // Fast renewal for testing
			OnWork: func(ctx context.Context, work *rtcqueue.Work, cancel <-chan rtcqueue.CancelMessage) error {
				// Track this worker as active
				current := activeWorkers.Add(1)
				mu.Lock()
				processingWorkers[work.WorkerID] = true
				// Update max concurrent if needed
				for {
					old := maxConcurrent.Load()
					if current <= old || maxConcurrent.CompareAndSwap(old, current) {
						break
					}
				}
				mu.Unlock()

				// Simulate work processing
				time.Sleep(50 * time.Millisecond)

				// Check for cancellation
				select {
				case <-cancel:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				// Mark work as done
				processedCount.Add(1)

				// Remove from active workers
				mu.Lock()
				delete(processingWorkers, work.WorkerID)
				mu.Unlock()
				activeWorkers.Add(-1)

				return nil
			},
		})
		workers[i] = w
	}

	// Start all workers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(worker *rtcqueue.Worker) {
			defer wg.Done()
			_ = worker.Run(ctx)
		}(w)
	}

	// Wait for workers to subscribe
	time.Sleep(200 * time.Millisecond)

	// Publish multiple work items to the same session
	for i := 0; i < numWorkItems; i++ {
		_, err := q.Publish(ctx, sessionID, fmt.Sprintf("work-%d", i), int64(i))
		if err != nil {
			t.Fatalf("publish work %d: %v", i, err)
		}
	}

	// Wait for all work to be processed or timeout
	deadline := time.After(workerTimeout)
	for {
		if processedCount.Load() >= int64(numWorkItems) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout: only %d/%d work items processed", processedCount.Load(), numWorkItems)
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Verify no concurrent processing occurred
	maxSeen := maxConcurrent.Load()
	if maxSeen > 1 {
		t.Errorf("concurrent processing detected: max concurrent workers = %d, want 1", maxSeen)
	}

	// Verify all work was processed
	if processedCount.Load() != int64(numWorkItems) {
		t.Errorf("processed %d work items, want %d", processedCount.Load(), numWorkItems)
	}

	// Cleanup
	cancel()
	wg.Wait()
}

// TestReleaseSessionCredentialCheck verifies that ReleaseSession with wrong
// credential does not delete another worker's lock.
func TestReleaseSessionCredentialCheck(t *testing.T) {
	q, mr := newTestQueue(t)
	defer mr.Close()

	ctx := context.Background()
	sessionID := "credential-test-session"
	workerID1 := "worker-1"
	workerID2 := "worker-2"

	// Publish a work item so we can claim it
	_, err := q.Publish(ctx, sessionID, "work-1", 5)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Worker 1 claims the lock
	claim1, err := q.ClaimWithCredential(ctx, sessionID, workerID1, "")
	if err != nil {
		t.Fatalf("worker1 claim: %v", err)
	}
	if claim1 == nil {
		t.Fatal("worker1 claim returned nil")
	}
	credential1 := claim1.Credential

	// Worker 1 completes the work (hold lock mode)
	if err := q.CompleteWork(ctx, claim1.WorkID); err != nil {
		t.Fatalf("worker1 complete: %v", err)
	}

	// Worker 2 tries to release worker 1's lock with wrong credential
	released, err := q.ReleaseSession(ctx, sessionID, workerID2, "wrong-credential")
	if err != nil {
		t.Fatalf("worker2 release: %v", err)
	}
	if released {
		t.Error("worker2 should NOT be able to release worker1's lock")
	}

	// Verify worker 1's lock still exists by trying to claim with worker 2
	// (should fail because worker 1 still holds the lock)
	// First publish another work item
	_, err = q.Publish(ctx, sessionID, "work-2", 5)
	if err != nil {
		t.Fatalf("publish work-2: %v", err)
	}
	claim2, err := q.ClaimWithCredential(ctx, sessionID, workerID2, "")
	if err != nil {
		t.Fatalf("worker2 claim attempt: %v", err)
	}
	if claim2 != nil {
		t.Error("worker2 should NOT be able to claim while worker1 holds lock")
	}

	// Worker 1 releases its own lock with correct credential
	released, err = q.ReleaseSession(ctx, sessionID, workerID1, credential1)
	if err != nil {
		t.Fatalf("worker1 release: %v", err)
	}
	if !released {
		t.Error("worker1 should be able to release its own lock")
	}

	// Now worker 2 should be able to claim
	claim3, err := q.ClaimWithCredential(ctx, sessionID, workerID2, "")
	if err != nil {
		t.Fatalf("worker2 claim after release: %v", err)
	}
	if claim3 == nil {
		t.Error("worker2 should be able to claim after worker1 released")
	}
}

// TestForceReleaseSession verifies that ForceReleaseSession works without
// credential check (for recovery scenarios).
func TestForceReleaseSession(t *testing.T) {
	q, mr := newTestQueue(t)
	defer mr.Close()

	ctx := context.Background()
	sessionID := "force-release-test-session"
	workerID := "test-worker"

	// Publish a work item so we can claim it
	_, err := q.Publish(ctx, sessionID, "work-1", 5)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Claim the lock
	claim, err := q.ClaimWithCredential(ctx, sessionID, workerID, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim == nil {
		t.Fatal("claim returned nil")
	}

	// Verify lock exists by trying to claim with another worker
	_, err = q.Publish(ctx, sessionID, "work-2", 5)
	if err != nil {
		t.Fatalf("publish work-2: %v", err)
	}
	otherClaim, err := q.ClaimWithCredential(ctx, sessionID, "other-worker", "")
	if err != nil {
		t.Fatalf("other claim attempt: %v", err)
	}
	if otherClaim != nil {
		t.Fatal("other worker should not claim while lock is held")
	}

	// Force release (no credential needed)
	if err := q.ForceReleaseSession(ctx, sessionID); err != nil {
		t.Fatalf("force release: %v", err)
	}

	// Verify lock is gone by successfully claiming with another worker
	newClaim, err := q.ClaimWithCredential(ctx, sessionID, "new-worker", "")
	if err != nil {
		t.Fatalf("new claim after force release: %v", err)
	}
	if newClaim == nil {
		t.Error("new worker should be able to claim after force release")
	}
}

// TestHoldLockRaceCondition simulates the exact race condition from the bug
// report: one worker releases lock, another claims it, then the first worker
// tries to release again (should not delete the second worker's lock).
func TestHoldLockRaceCondition(t *testing.T) {
	q, mr := newTestQueue(t)
	defer mr.Close()

	ctx := context.Background()
	sessionID := "race-condition-session"
	worker1 := "worker-1"
	worker2 := "worker-2"

	// Publish work items
	_, err := q.Publish(ctx, sessionID, "work-1", 5)
	if err != nil {
		t.Fatalf("publish work-1: %v", err)
	}

	// Step 1: Worker 1 claims the lock
	claim1, err := q.ClaimWithCredential(ctx, sessionID, worker1, "")
	if err != nil {
		t.Fatalf("worker1 claim: %v", err)
	}
	if claim1 == nil {
		t.Fatal("worker1 claim returned nil")
	}
	cred1 := claim1.Credential

	// Step 2: Worker 1 completes work (hold lock mode)
	if err := q.CompleteWork(ctx, claim1.WorkID); err != nil {
		t.Fatalf("worker1 complete: %v", err)
	}

	// Step 3: Worker 1 releases lock (simulating doCleanup)
	released, err := q.ReleaseSession(ctx, sessionID, worker1, cred1)
	if err != nil {
		t.Fatalf("worker1 release: %v", err)
	}
	if !released {
		t.Error("worker1 should release its lock")
	}

	// Step 4: Worker 2 claims the lock (simulating notification wake-up)
	_, err = q.Publish(ctx, sessionID, "work-2", 5)
	if err != nil {
		t.Fatalf("publish work-2: %v", err)
	}
	claim2, err := q.ClaimWithCredential(ctx, sessionID, worker2, "")
	if err != nil {
		t.Fatalf("worker2 claim: %v", err)
	}
	if claim2 == nil {
		t.Fatal("worker2 claim returned nil")
	}
	cred2 := claim2.Credential

	// Step 5: Worker 1 tries to release again (simulating processSessionHoldLock cleanup)
	// This should NOT delete worker2's lock
	released, err = q.ReleaseSession(ctx, sessionID, worker1, cred1)
	if err != nil {
		t.Fatalf("worker1 second release: %v", err)
	}
	if released {
		t.Error("worker1 should NOT release lock it no longer owns")
	}

	// Step 6: Verify worker2's lock still exists by trying to claim with worker1
	// (should fail because worker2 still holds the lock)
	_, err = q.Publish(ctx, sessionID, "work-3", 5)
	if err != nil {
		t.Fatalf("publish work-3: %v", err)
	}
	claim3, err := q.ClaimWithCredential(ctx, sessionID, worker1, cred1)
	if err != nil {
		t.Fatalf("worker1 claim attempt: %v", err)
	}
	if claim3 != nil {
		t.Error("worker1 should NOT be able to claim while worker2 holds lock")
	}

	// Step 7: Verify worker2 can still renew its lock
	ok, err := q.RenewLockWithCredential(ctx, sessionID, worker2, cred2)
	if err != nil {
		t.Fatalf("worker2 renew: %v", err)
	}
	if !ok {
		t.Error("worker2 should be able to renew its lock")
	}

	// Step 8: Worker 2 releases its own lock
	released, err = q.ReleaseSession(ctx, sessionID, worker2, cred2)
	if err != nil {
		t.Fatalf("worker2 release: %v", err)
	}
	if !released {
		t.Error("worker2 should release its own lock")
	}
}
