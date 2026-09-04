package rtcqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerConfig configures a Worker that manages the full lifecycle of
// processing work items: subscribing to notifications, claiming, lock
// renewal, cancel handling, draining remaining work in a session, and
// graceful shutdown. The caller only provides a callback for the actual
// work logic.
type WorkerConfig struct {
	// WorkerID uniquely identifies this worker. Used for lock ownership.
	WorkerID string

	// Concurrency is the maximum number of sessions this worker will
	// process concurrently. Defaults to 1 if zero.
	Concurrency int

	// OnWork is called for each claimed work item. The context is
	// cancelled when the worker is shutting down or when the session
	// lock is lost. The cancel channel receives a CancelMessage when
	// the work is cancelled by an admin; the callback should abort and
	// return. OnWork should return nil on success or an error on
	// failure (the error is passed to OnError if set, and the work is
	// left in "processing" state for manual recovery).
	OnWork func(ctx context.Context, work *Work, cancel <-chan CancelMessage) error

	// OnError is called when OnWork returns an error or when an internal
	// error occurs (e.g. Redis failure). If nil, errors are logged to
	// the standard logger.
	OnError func(err error)

	// RenewInterval is how often the worker renews the session lock.
	// Defaults to DefaultRenewIntervalSec (30s). Set shorter for tests.
	RenewInterval time.Duration
}

// Worker manages the lifecycle of processing work items from a Queue.
// It subscribes to session:new notifications, claims work, renews the
// lock periodically, listens for cancel notifications, drains remaining
// work in the same session, and supports graceful shutdown.
type Worker struct {
	q   *Queue
	cfg WorkerConfig

	mu       sync.Mutex
	running  bool
	sessions map[string]context.CancelFunc // active session processors
	wg       sync.WaitGroup
	sem      chan struct{} // concurrency semaphore
}

// NewWorker constructs a Worker. Call Run to start processing.
func NewWorker(q *Queue, cfg WorkerConfig) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.RenewInterval <= 0 {
		cfg.RenewInterval = DefaultRenewIntervalSec * time.Second
	}
	if cfg.OnError == nil {
		cfg.OnError = func(err error) {
			log.Printf("[rtcqueue.Worker] error: %v", err)
		}
	}
	return &Worker{
		q:        q,
		cfg:      cfg,
		sessions: make(map[string]context.CancelFunc),
		sem:      make(chan struct{}, cfg.Concurrency),
	}
}

// logIfEnabled logs a message for debugging worker lifecycle.
func (w *Worker) logIfEnabled(event string, fields map[string]any) {
	// Simple debug logging - in production use a proper logger
	log.Printf("[rtcqueue.Worker] %s: %v", event, fields)
}

// Run starts the worker loop. It blocks until ctx is cancelled or an
// unrecoverable error occurs. Run subscribes to session:new and
// dispatches work to OnWork callbacks. Call Stop for graceful shutdown.
func (w *Worker) Run(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return fmt.Errorf("rtcqueue: worker already running")
	}
	w.running = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	sub := w.q.SubscribeNew(ctx)
	defer sub.Close()
	ch := sub.Channel()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			sessionID := msg.Payload
			w.logIfEnabled("worker.received_notification", map[string]any{
				"session_id": sessionID,
			})
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				// acquire semaphore slot (blocks if at concurrency limit)
				select {
				case w.sem <- struct{}{}:
					defer func() { <-w.sem }()
				case <-ctx.Done():
					return
				}
				w.processSession(ctx, sessionID)
			}()
		}
	}
}

// Stop gracefully shuts down the worker. It cancels all active session
// processors and waits for them to finish, or until ctx expires
// (whichever comes first). After Stop returns, the worker cannot be
// restarted.
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return nil
	}
	// cancel all active sessions
	for _, cancel := range w.sessions {
		cancel()
	}
	w.mu.Unlock()

	// wait for all goroutines to finish, with timeout
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// processSession claims ONE work item from the session, processes it,
// and returns. After Complete releases the lock, all workers compete
// again for the next notification. This ensures fairer load distribution
// across the cluster.
func (w *Worker) processSession(globalCtx context.Context, sessionID string) {
	// create a session-scoped context so we can cancel this session
	// independently (e.g. on Stop)
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	w.mu.Lock()
	w.sessions[sessionID] = cancel
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.sessions, sessionID)
		w.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return
	default:
	}

	w.logIfEnabled("worker.claiming", map[string]any{
		"session_id": sessionID,
		"worker_id":  w.cfg.WorkerID,
	})

	claim, err := w.q.Claim(ctx, sessionID, w.cfg.WorkerID)
	if err != nil {
		w.logIfEnabled("worker.claim_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		w.cfg.OnError(fmt.Errorf("claim session %s: %w", sessionID, err))
		return
	}
	if claim == nil {
		// queue empty or lost the race
		w.logIfEnabled("worker.claim_empty", map[string]any{
			"session_id": sessionID,
		})
		return
	}

	w.logIfEnabled("worker.claimed", map[string]any{
		"session_id": sessionID,
		"work_id":    claim.WorkID,
	})

	w.processWork(ctx, claim)
	// after processWork returns, the lock is released (by Complete).
	// We return here — no draining. The next session:new notification
	// will trigger a fresh claim, and all workers compete again.
}

// processWork handles a single work item: starts lock renewal, listens
// for cancel, calls OnWork, and completes if successful. The lock is
// released by Complete (on success) or left to expire (on error/lock-loss).
func (w *Worker) processWork(ctx context.Context, claim *ClaimResult) {
	w.logIfEnabled("worker.processing_work", map[string]any{
		"work_id":    claim.WorkID,
		"session_id": claim.SessionID,
	})

	work, err := w.q.LoadWork(ctx, claim.WorkID)
	if err != nil {
		w.cfg.OnError(fmt.Errorf("load work %s: %w", claim.WorkID, err))
		return
	}
	if work == nil {
		w.cfg.OnError(fmt.Errorf("work %s vanished", claim.WorkID))
		return
	}

	w.logIfEnabled("worker.loaded_work", map[string]any{
		"work_id":    claim.WorkID,
		"session_id": work.SessionID,
	})

	// cancel channel for this work
	cancelCh := make(chan CancelMessage, 1)
	var adminCancelled atomic.Bool

	// workCtx is cancelled when: the session ctx is cancelled, OR the
	// lock is lost, OR the admin cancels this work. OnWork should watch
	// both workCtx.Done() and cancelCh.
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()

	// subscribe to cancel notifications
	cancelSub := w.q.SubscribeCancel(workCtx, claim.SessionID)
	defer cancelSub.Close()
	go func() {
		for msg := range cancelSub.Channel() {
			var cm CancelMessage
			if err := json.Unmarshal([]byte(msg.Payload), &cm); err != nil {
				continue
			}
			if cm.WorkID == claim.WorkID {
				select {
				case cancelCh <- cm:
				default:
				}
				adminCancelled.Store(true)
				workCancel() // abort OnWork
				return
			}
		}
	}()

	// lock renewal ticker — tracks whether we still own the lock
	var lockLost atomic.Bool
	renewDone := make(chan struct{})
	go func() {
		t := time.NewTicker(w.cfg.RenewInterval)
		defer t.Stop()
		for {
			select {
			case <-renewDone:
				return
			case <-workCtx.Done():
				return
			case <-t.C:
				ok, err := w.q.RenewLock(workCtx, claim.SessionID, w.cfg.WorkerID)
				if err != nil {
					w.cfg.OnError(fmt.Errorf("renew lock: %w", err))
				}
				if !ok {
					lockLost.Store(true)
					workCancel() // abort OnWork — we no longer own this work
					return
				}
			}
		}
	}()
	defer close(renewDone)

	// call the user's callback
	w.logIfEnabled("worker.calling_onwork", map[string]any{
		"work_id":    work.ID,
		"session_id": work.SessionID,
	})
	err = w.cfg.OnWork(workCtx, work, cancelCh)
	w.logIfEnabled("worker.onwork_returned", map[string]any{
		"work_id":    work.ID,
		"session_id": work.SessionID,
		"error":      fmt.Sprintf("%v", err),
	})
	if lockLost.Load() {
		// another worker took over; do NOT call Complete (it would release
		// someone else's lock). The lock will expire naturally.
		return
	}
	if adminCancelled.Load() {
		// admin cancelled; the cancel script already set status=cancelled
		// and released the lock. Do NOT call Complete.
		return
	}
	if err != nil {
		w.cfg.OnError(fmt.Errorf("onwork %s: %w", claim.WorkID, err))
		// leave work in "processing" state for manual recovery
		return
	}

	// success — complete the work (also releases the session lock).
	// Use a fresh context so the Complete call succeeds even if the
	// parent ctx was cancelled (e.g. during graceful shutdown).
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer completeCancel()
	if err := w.q.Complete(completeCtx, claim.WorkID); err != nil {
		w.cfg.OnError(fmt.Errorf("complete %s: %w", claim.WorkID, err))
	}
}
