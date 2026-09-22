package rtcqueue

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// WorkerConfig configures a Worker that manages the full lifecycle of
// processing work items: subscribing to notifications, claiming, lock
// renewal, cancel handling, draining remaining work in a session, and
// graceful shutdown. The caller only provides a callback for the actual
// work logic.
//
// Note: Worker is the current production lifecycle manager, wired via
// cmd/wire.go. The primitive API (Claim, ClaimWithCredential,
// CompleteWork, ReleaseSession) is available for building custom
// lifecycle management when needed (e.g., "hold lock" mode).
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

	// Logger provides structured logging for worker lifecycle events.
	// If nil, falls back to the standard library log package.
	Logger WorkerLogger

	// HoldLock enables "hold lock" mode where the worker keeps the session
	// lock after completing a work item and continues to claim more work
	// from the same session. When the queue is empty, the lock is released.
	// This is required for turn-loop agents that need to process multiple
	// work items for the same session without releasing the lock.
	//
	// When enabled, the worker uses ClaimWithCredential and CompleteWork
	// instead of Claim and Complete.
	HoldLock bool
}

// WorkerLogger is the logging interface used by Worker.
// Compatible with most structured loggers via adapter.
type WorkerLogger interface {
	Info(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}

// Worker manages the lifecycle of processing work items from a Queue.
// It subscribes to session:new notifications, claims work, renews the
// lock periodically, listens for cancel notifications, drains remaining
// work in the same session, and supports graceful shutdown.
type Worker struct {
	q   *Queue
	cfg WorkerConfig

	mu            sync.Mutex
	running       bool
	sessions      map[string]context.CancelFunc // active session processors
	sessionClaims sync.Mutex                    // prevents concurrent processSession for the same session
	wg            sync.WaitGroup
	sem           chan struct{} // concurrency semaphore
}

// workerTracer returns the tracer for worker operations.
func workerTracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("rtc-queue.worker")
}

// NewWorker constructs a Worker. Call Run to start processing.
func NewWorker(q *Queue, cfg WorkerConfig) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.RenewInterval <= 0 {
		cfg.RenewInterval = DefaultRenewIntervalSec * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = noopWorkerLogger{}
	}
	if cfg.OnError == nil {
		cfg.OnError = func(err error) {
			cfg.Logger.Error("worker error", "error", err)
		}
	}
	return &Worker{
		q:        q,
		cfg:      cfg,
		sessions: make(map[string]context.CancelFunc),
		sem:      make(chan struct{}, cfg.Concurrency),
	}
}

// log logs a message for debugging worker lifecycle.
// Keys are sorted for deterministic output order (Go map iteration is random).
func (w *Worker) log(event string, fields map[string]any) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kv := make([]any, 0, len(fields)*2)
	for _, k := range keys {
		kv = append(kv, k, fields[k])
	}
	w.cfg.Logger.Info(event, kv...)
}

// logError logs an error message using the configured logger.
func (w *Worker) logError(msg string, keysAndValues ...any) {
	w.cfg.Logger.Error(msg, keysAndValues...)
}

// Run starts the worker loop. It blocks until ctx is cancelled or an
// unrecoverable error occurs. Run subscribes to session:new and
// dispatches work to OnWork callbacks. Call Stop for graceful shutdown.
func (w *Worker) Run(ctx context.Context) error {
	ctx, span := workerTracer().Start(ctx, "Worker.Run",
		trace.WithAttributes(
			attribute.String("worker.id", w.cfg.WorkerID),
			attribute.Int("worker.concurrency", w.cfg.Concurrency),
		),
	)
	defer span.End()

	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		span.SetStatus(codes.Error, "worker already running")
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
	defer func() { _ = sub.Close() }()
	ch := sub.Channel()

	for {
		select {
		case <-ctx.Done():
			span.SetAttributes(attribute.String("worker.exit_reason", "context_cancelled"))
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				span.SetAttributes(attribute.String("worker.exit_reason", "channel_closed"))
				return nil
			}
			sessionID := msg.Payload
			w.log("worker.received_notification", map[string]any{
				"session_id": sessionID,
			})
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				defer func() {
					if r := recover(); r != nil {
						w.logError("goroutine panic",
							"session", sessionID, "recover", r, "stack", string(debug.Stack()))
					}
				}()
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
	ctx, span := workerTracer().Start(ctx, "Worker.Stop",
		trace.WithAttributes(
			attribute.String("worker.id", w.cfg.WorkerID),
		),
	)
	defer span.End()

	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		span.SetAttributes(attribute.Bool("worker.was_running", false))
		return nil
	}
	// cancel all active sessions
	sessionCount := len(w.sessions)
	for _, cancel := range w.sessions {
		if cancel != nil { // guard against nil during processSession startup race
			cancel()
		}
	}
	w.mu.Unlock()
	span.SetAttributes(
		attribute.Bool("worker.was_running", true),
		attribute.Int("worker.sessions_cancelled", sessionCount),
	)

	// wait for all goroutines to finish, with timeout
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				w.logError("stop wait panic",
					"recover", r, "stack", string(debug.Stack()))
			}
		}()
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		span.SetAttributes(attribute.Bool("worker.stopped_gracefully", true))
		return nil
	case <-ctx.Done():
		span.SetAttributes(attribute.Bool("worker.stopped_gracefully", false))
		span.SetStatus(codes.Error, "stop timed out")
		return ctx.Err()
	}
}

// processSession claims ONE work item from the session, processes it,
// and returns. After Complete releases the lock, all workers compete
// again for the next notification. This ensures fairer load distribution
// across the cluster.
//
// When HoldLock is enabled, the worker keeps the session lock after
// completing a work item and continues to claim more work from the
// same session until the queue is empty.
func (w *Worker) processSession(globalCtx context.Context, sessionID string) {
	ctx, span := workerTracer().Start(globalCtx, "Worker.processSession",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("worker.id", w.cfg.WorkerID),
			attribute.Bool("worker.hold_lock", w.cfg.HoldLock),
		),
	)
	defer span.End()

	// Prevent concurrent processSession for the same session.
	// If another goroutine is already processing this session, skip.
	// This prevents a race where two goroutines both call ClaimWithCredential
	// with empty credentials, each creating a lock with different credentials,
	// leading to lock-loss detection and unexpected turn cancellation.
	w.sessionClaims.Lock()
	w.mu.Lock()
	_, active := w.sessions[sessionID]
	w.mu.Unlock()
	if active {
		w.sessionClaims.Unlock()
		span.SetAttributes(attribute.Bool("worker.skipped", true))
		w.log("worker.session_already_active", map[string]any{
			"session_id": sessionID,
			"message":    "skipping concurrent processSession",
		})
		return
	}
	// Mark session as active (will be cleared in the deferred cleanup).
	// Hold w.mu to prevent a data race with Stop(), which iterates
	// w.sessions under w.mu. Without this, concurrent map write (here)
	// and map read (Stop) would panic at runtime.
	w.mu.Lock()
	w.sessions[sessionID] = nil
	w.mu.Unlock()
	w.sessionClaims.Unlock()

	// create a session-scoped context so we can cancel this session
	// independently (e.g. on Stop)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Update the sessions map with the actual cancel function
	w.mu.Lock()
	w.sessions[sessionID] = cancel
	w.mu.Unlock()

	defer func() {
		// Clear from both maps
		w.mu.Lock()
		delete(w.sessions, sessionID)
		w.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		span.SetAttributes(attribute.String("worker.exit_reason", "context_cancelled"))
		return
	default:
	}

	w.log("worker.claiming", map[string]any{
		"session_id": sessionID,
		"worker_id":  w.cfg.WorkerID,
	})

	if w.cfg.HoldLock {
		// Hold lock mode: use ClaimWithCredential
		w.processSessionHoldLock(ctx, sessionID)
	} else {
		// Normal mode: use Claim
		w.processSessionNormal(ctx, sessionID)
	}
}

// processSessionNormal handles the normal mode: claim one work, process it, release lock.
func (w *Worker) processSessionNormal(ctx context.Context, sessionID string) {
	ctx, span := workerTracer().Start(ctx, "Worker.processSessionNormal",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
		),
	)
	defer span.End()

	claim, err := w.q.Claim(ctx, sessionID, w.cfg.WorkerID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		w.log("worker.claim_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		w.cfg.OnError(fmt.Errorf("claim session %s: %w", sessionID, err))
		return
	}
	if claim == nil {
		// queue empty or lost the race
		span.SetAttributes(attribute.Bool("queue.claimed", false))
		w.log("worker.claim_empty", map[string]any{
			"session_id": sessionID,
		})
		return
	}

	span.SetAttributes(
		attribute.Bool("queue.claimed", true),
		attribute.String("work.id", claim.WorkID),
	)
	w.log("worker.claimed", map[string]any{
		"session_id": sessionID,
		"work_id":    claim.WorkID,
	})

	w.processWork(ctx, claim)
	// after processWork returns, the lock is released (by Complete).
	// We return here — no draining. The next session:new notification
	// will trigger a fresh claim, and all workers compete again.
}

// processSessionHoldLock handles the hold lock mode: claim work with credential,
// process it, complete without releasing lock, continue until queue is empty.
func (w *Worker) processSessionHoldLock(ctx context.Context, sessionID string) {
	ctx, span := workerTracer().Start(ctx, "Worker.processSessionHoldLock",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
		),
	)
	defer span.End()

	// First claim: pass empty credential, get credential from result
	claim, err := w.q.ClaimWithCredential(ctx, sessionID, w.cfg.WorkerID, "")
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		w.log("worker.claim_failed", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		w.cfg.OnError(fmt.Errorf("claim session %s: %w", sessionID, err))
		return
	}
	if claim == nil {
		// queue empty or lost the race
		span.SetAttributes(attribute.Bool("queue.claimed", false))
		w.log("worker.claim_empty", map[string]any{
			"session_id": sessionID,
		})
		return
	}

	span.SetAttributes(
		attribute.Bool("queue.claimed", true),
		attribute.String("work.id", claim.WorkID),
	)
	w.log("worker.claimed", map[string]any{
		"session_id": sessionID,
		"work_id":    claim.WorkID,
		"credential": claim.Credential,
	})

	credential := claim.Credential
	workCount := 0

	// Process work in a loop
	for {
		w.processWorkHoldLock(ctx, claim)
		workCount++

		// Try to claim next work with credential
		nextClaim, err := w.q.ClaimWithCredential(ctx, sessionID, w.cfg.WorkerID, credential)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			w.log("worker.claim_next_failed", map[string]any{
				"session_id": sessionID,
				"error":      err.Error(),
			})
			// Release lock on error. Use Background() with timeout because ctx
			// may be cancelled (worker shutdown), which would prevent the lock
			// from being released and leave the session locked until TTL expires.
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := w.q.ReleaseSession(releaseCtx, sessionID); err != nil {
				w.logError("worker.release_session_failed",
					"session", sessionID, "error", err.Error())
			}
			releaseCancel()
			span.SetAttributes(attribute.Int("worker.works_processed", workCount))
			return
		}
		if nextClaim == nil {
			// Queue is empty, release lock.
			// Use Background() with timeout for the same reason as above.
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
			w.log("worker.queue_empty_releasing_lock", map[string]any{
				"session_id": sessionID,
			})
			if err := w.q.ReleaseSession(releaseCtx, sessionID); err != nil {
				w.logError("worker.release_session_failed",
					"session", sessionID, "error", err.Error())
			}
			releaseCancel()
			span.SetAttributes(attribute.Int("worker.works_processed", workCount))
			return
		}

		w.log("worker.claimed_next", map[string]any{
			"session_id": sessionID,
			"work_id":    nextClaim.WorkID,
		})
		claim = nextClaim
	}
}

// processWork handles a single work item: starts lock renewal, listens
// for cancel, calls OnWork, and completes if successful. The lock is
// released by Complete (on success) or left to expire (on error/lock-loss).
func (w *Worker) processWork(ctx context.Context, claim *ClaimResult) {
	w.processWorkInternal(ctx, claim, false, "")
}

// processWorkHoldLock handles a single work item in hold-lock mode.
// The lock is NOT released after completion; instead, CompleteWork is called.
func (w *Worker) processWorkHoldLock(ctx context.Context, claim *ClaimResult) {
	w.processWorkInternal(ctx, claim, true, claim.Credential)
}

// processWorkInternal is the shared implementation for both normal and hold-lock modes.
func (w *Worker) processWorkInternal(ctx context.Context, claim *ClaimResult, holdLock bool, credential string) {
	ctx, span := workerTracer().Start(ctx, "Worker.processWorkInternal",
		trace.WithAttributes(
			attribute.String("work.id", claim.WorkID),
			attribute.String("session.id", claim.SessionID),
			attribute.Bool("worker.hold_lock", holdLock),
		),
	)
	defer span.End()

	w.log("worker.processing_work", map[string]any{
		"work_id":    claim.WorkID,
		"session_id": claim.SessionID,
		"hold_lock":  holdLock,
	})

	// Use a timeout for the Redis call to prevent blocking indefinitely
	// if Redis is unresponsive. Consistent with other Redis calls in this
	// file (e.g., lock release at lines 363, 374).
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer loadCancel()
	work, err := w.q.LoadWork(loadCtx, claim.WorkID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		w.cfg.OnError(fmt.Errorf("load work %s: %w", claim.WorkID, err))
		return
	}
	if work == nil {
		span.SetStatus(codes.Error, "work vanished")
		w.cfg.OnError(fmt.Errorf("work %s vanished", claim.WorkID))
		return
	}

	// Attach the credential from the claim so downstream consumers
	// (e.g. turn-agent) can use it without re-claiming the lock.
	work.Credential = credential

	w.log("worker.loaded_work", map[string]any{
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

	// Safety-net check + cancel subscription setup (extracted for complexity).
	w.setupCancelListener(workCtx, claim, work, cancelCh, &adminCancelled, workCancel)

	// Lock renewal goroutine (extracted for complexity).
	lockLost := w.startLockRenewal(workCtx, claim, holdLock, credential, workCancel)
	defer close(lockLost.done)

	// call the user's callback
	w.log("worker.calling_onwork", map[string]any{
		"work_id":    work.ID,
		"session_id": work.SessionID,
	})
	err = w.cfg.OnWork(workCtx, work, cancelCh)
	onworkFields := map[string]any{
		"work_id":    work.ID,
		"session_id": work.SessionID,
	}
	if err != nil {
		onworkFields["error"] = err.Error()
		span.SetStatus(codes.Error, err.Error())
	}
	w.log("worker.onwork_returned", onworkFields)

	// Handle completion (extracted for complexity).
	w.handleWorkCompletion(claim, err, holdLock, lockLost.lost, &adminCancelled)
}
