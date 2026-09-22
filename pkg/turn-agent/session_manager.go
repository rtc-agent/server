package turnagent

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// sessionRenewalInterval is how often the session lock is renewed.
// Must be well below DefaultLockTTLSeconds (120s) to prevent expiry.
const sessionRenewalInterval = 30 * time.Second

// SessionTurnManager manages the lifecycle of a single session's turn loop.
//
// A SessionTurnManager is created per session (not per turn). It holds the
// session lock credential, runs the eino TurnLoop with UntilIdleFor(100ms),
// and handles work completion and cleanup.
//
// Lifecycle:
//
//	mgr, err := NewSessionTurnManager(...)
//	// Push work items and wait for completion
//	exitState := mgr.Wait()
//	// cleanup runs automatically after Wait returns
type SessionTurnManager struct {
	sessionID string
	workerID  string
	turnID    string // turnID for work items
	queue     *rtcqueue.Queue
	cfg       Config
	tracker   *WorkTracker
	registry  *SessionManagerRegistry // back-reference for cleanup

	credMu     sync.Mutex
	credential string // lock credential from initial ClaimWithCredential

	loop *adk.TurnLoop[TurnWorkItem, *schema.Message]

	// lastMessage tracks the last assistant message for Sub Agent support.
	lastMsgMu   sync.Mutex
	lastMessage *Message

	// logFn is an optional closure for structured logging.
	logFn func(ctx context.Context, level LogLevel, msg string, fields map[string]any)

	renewCancelMu sync.Mutex         // protects renewCancel from concurrent access
	renewCancel   context.CancelFunc // set by Run, called by doCleanup
	done          chan struct{}      // closed when cleanup completes
	cleanupOnce   sync.Once          // ensures cleanup runs exactly once

	// loopExitState is set after Wait() returns.
	// waitOnce ensures loop.Wait() is called exactly once, even when both
	// the monitor goroutine (in Run) and the owner's Process call mgr.Wait()
	// concurrently. Without this, one caller could block forever if eino's
	// TurnLoop.Wait() is a one-shot call.
	loopExitMu    sync.Mutex
	loopExitState *adk.TurnLoopExitState[TurnWorkItem, *schema.Message]
	waitOnce      sync.Once

	// cancelledByQueue is set when a cancel message arrives from rtc-queue.
	cancelMu         sync.Mutex
	cancelledByQueue bool
	cancelReason     string

	// lockLost is set when lock renewal detects the lock was lost (expired or
	// taken by another worker). When true, cleanup must NOT call ReleaseSession
	// because another worker may have already acquired a new lock at the same
	// Redis key. Deleting it would cause a split-brain scenario.
	lockLost atomic.Bool

	// consecutiveRenewFailures counts consecutive Redis errors during lock renewal.
	// Only when this reaches maxConsecutiveRenewFailures is lockLost set to true.
	// Transient errors (network blips, connection pool exhaustion) are tolerated
	// up to the threshold; only ok==false (definite lock loss) triggers immediately.
	consecutiveRenewFailures atomic.Int64

	// checkpointID is the eino checkpoint key for this session.
	checkpointID string

	// resumeCancelMu protects resumeCancel, which holds the cancel function from
	// the most recent GenResume context.WithTimeout call. doCleanup calls it to
	// prevent timer goroutine leaks when the manager exits before the next resume.
	resumeCancelMu sync.Mutex
	resumeCancel   context.CancelFunc
}

// NewSessionTurnManager creates a SessionTurnManager with an existing lock credential.
func NewSessionTurnManager(
	ctx context.Context,
	queue *rtcqueue.Queue,
	sessionID, workerID, credential, turnID, checkpointID string,
	cfg Config,
	tracker *WorkTracker,
	registry *SessionManagerRegistry,
	logFn func(ctx context.Context, level LogLevel, msg string, fields map[string]any),
) (*SessionTurnManager, error) {
	if credential == "" {
		return nil, fmt.Errorf("turnagent: credential is required")
	}

	mgr := &SessionTurnManager{
		sessionID:    sessionID,
		workerID:     workerID,
		turnID:       turnID,
		queue:        queue,
		cfg:          cfg,
		tracker:      tracker,
		registry:     registry,
		credential:   credential,
		checkpointID: checkpointID,
		logFn:        logFn,
		done:         make(chan struct{}),
	}

	// Build the eino TurnLoop config.
	einoCfg := mgr.buildEinoConfig()
	mgr.loop = adk.NewTurnLoop[TurnWorkItem, *schema.Message](einoCfg)

	mgr.log(ctx, LogLevelInfo, "session_manager.created", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
		"worker_id":  workerID,
	})

	return mgr, nil
}

// Loop returns the underlying eino TurnLoop.
func (mgr *SessionTurnManager) Loop() *adk.TurnLoop[TurnWorkItem, *schema.Message] {
	return mgr.loop
}

// Tracker returns the work tracker.
func (mgr *SessionTurnManager) Tracker() *WorkTracker {
	return mgr.tracker
}

// Done returns a channel that is closed when the manager has fully stopped.
func (mgr *SessionTurnManager) Done() <-chan struct{} {
	return mgr.done
}

// Credential returns the current lock credential.
func (mgr *SessionTurnManager) Credential() string {
	return mgr.getCredential()
}

// SessionID returns the session this manager is managing.
func (mgr *SessionTurnManager) SessionID() string {
	return mgr.sessionID
}

// TurnID returns the turn ID for this manager.
func (mgr *SessionTurnManager) TurnID() string {
	return mgr.turnID
}

// UpdateCredential updates the lock credential from an external caller.
func (mgr *SessionTurnManager) UpdateCredential(cred string) {
	if cred == "" {
		return
	}
	mgr.credMu.Lock()
	defer mgr.credMu.Unlock()
	mgr.credential = cred
}

func (mgr *SessionTurnManager) getCredential() string {
	mgr.credMu.Lock()
	defer mgr.credMu.Unlock()
	return mgr.credential
}

// setLastMessage stores the last assistant message for Sub Agent support.
func (mgr *SessionTurnManager) setLastMessage(msg *Message) {
	mgr.lastMsgMu.Lock()
	defer mgr.lastMsgMu.Unlock()
	mgr.lastMessage = msg
}

// LastMessage returns the last assistant message produced by the turn.
func (mgr *SessionTurnManager) LastMessage() *Message {
	mgr.lastMsgMu.Lock()
	defer mgr.lastMsgMu.Unlock()
	return mgr.lastMessage
}

// Run starts the TurnLoop and lock renewal goroutine.
// This is non-blocking; the loop runs in a goroutine managed by eino.
func (mgr *SessionTurnManager) Run(ctx context.Context) {
	renewCtx, cancel := context.WithCancel(ctx)
	mgr.renewCancelMu.Lock()
	mgr.renewCancel = cancel
	mgr.renewCancelMu.Unlock()

	// Configure auto-exit: when the loop has been continuously idle for 100ms
	// (no items in buffer, blocking between turns), it exits automatically.
	// This must be set BEFORE Run because Run is blocking.
	mgr.loop.Stop(adk.UntilIdleFor(100 * time.Millisecond))

	// Start lock renewal goroutine.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				mgr.log(context.Background(), LogLevelError, "session_manager.run_lock_renewal_panic", map[string]any{
					"session_id": mgr.sessionID,
					"panic":      fmt.Sprintf("%v", r),
					"stack":      string(debug.Stack()),
				})
				// Treat panic as lock loss to prevent split-brain: the lock TTL
				// will expire while we can no longer renew it. Without this, the
				// turn loop would continue running without lock-loss detection,
				// potentially conflicting with a new worker that claims the session.
				mgr.lockLost.Store(true)
				mgr.loop.Stop(adk.WithSkipCheckpoint())
			}
		}()
		mgr.runLockRenewal(renewCtx)
	}()

	// Monitor goroutine: when the loop exits, automatically run cleanup.
	// This ensures cleanup happens even if nobody calls Wait().
	// Use context.Background() for cleanup so that a cancelled parent context
	// does not prevent critical cleanup operations (ReleaseSession, requeue).
	// Use mgr.Wait() (not mgr.loop.Wait()) so the sync.Once protection applies.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				mgr.log(context.Background(), LogLevelError, "session_manager.monitor_panic", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    mgr.turnID,
					"panic":      fmt.Sprintf("%v", r),
				})
			}
		}()
		mgr.Wait()
		// Use a timeout context to prevent the goroutine from hanging
		// indefinitely if Redis is unresponsive.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		mgr.Cleanup(cleanupCtx)
	}()

	// Start the TurnLoop. This blocks until the loop exits.
	mgr.loop.Run(ctx)
}

// Wait blocks until the TurnLoop exits and returns the exit state.
// Safe for concurrent use by both the monitor goroutine (in Run) and
// the owner's Process. Uses sync.Once to ensure loop.Wait() is called
// exactly once, preventing deadlock if eino's Wait is a one-shot call.
func (mgr *SessionTurnManager) Wait() *adk.TurnLoopExitState[TurnWorkItem, *schema.Message] {
	mgr.waitOnce.Do(func() {
		exitState := mgr.loop.Wait()
		mgr.loopExitMu.Lock()
		mgr.loopExitState = exitState
		mgr.loopExitMu.Unlock()
	})

	mgr.loopExitMu.Lock()
	defer mgr.loopExitMu.Unlock()
	return mgr.loopExitState
}

// Stop initiates a graceful shutdown of the manager.
func (mgr *SessionTurnManager) Stop(opts ...adk.StopOption) {
	mgr.loop.Stop(opts...)
}

// SetCancelledByQueue marks the manager as cancelled by the queue.
func (mgr *SessionTurnManager) SetCancelledByQueue(reason string) {
	mgr.cancelMu.Lock()
	defer mgr.cancelMu.Unlock()
	mgr.cancelledByQueue = true
	mgr.cancelReason = reason
}

// IsCancelledByQueue returns whether the manager was cancelled by the queue.
func (mgr *SessionTurnManager) IsCancelledByQueue() bool {
	mgr.cancelMu.Lock()
	defer mgr.cancelMu.Unlock()
	return mgr.cancelledByQueue
}

// CancelReason returns the cancel reason from the queue.
func (mgr *SessionTurnManager) CancelReason() string {
	mgr.cancelMu.Lock()
	defer mgr.cancelMu.Unlock()
	return mgr.cancelReason
}

// IsLockLost returns whether the session lock was lost (detected by lock renewal).
// When true, cleanup will skip ReleaseSession to avoid deleting another worker's lock.
func (mgr *SessionTurnManager) IsLockLost() bool {
	return mgr.lockLost.Load()
}

// Cleanup performs post-loop cleanup: claim remaining work, stop renewal,
// release the session lock, and remove from registry.
// Cleanup is idempotent — it runs at most once, even if called multiple times.
func (mgr *SessionTurnManager) Cleanup(ctx context.Context) {
	mgr.cleanupOnce.Do(func() {
		mgr.doCleanup(ctx)
	})
}

func (mgr *SessionTurnManager) doCleanup(ctx context.Context) {
	defer close(mgr.done)

	cleanupCtx, cleanupSpan := mgr.startSpanIfEnabled(ctx, "session_cleanup",
		trace.WithAttributes(
			attribute.String("session.id", mgr.sessionID),
			attribute.String("turn.id", mgr.turnID),
			attribute.Bool("lock.lost", mgr.lockLost.Load()),
		),
	)
	defer cleanupSpan.End()

	// Step 1: Complete all pending work in the tracker (unblock waiting Process calls).
	cleanupSpan.AddEvent("complete_all_work")
	mgr.tracker.CompleteAll()

	// Step 2: Stop lock renewal.
	cleanupSpan.AddEvent("stop_lock_renewal")
	mgr.renewCancelMu.Lock()
	if mgr.renewCancel != nil {
		mgr.renewCancel()
	}
	mgr.renewCancelMu.Unlock()

	// Step 2b: Cancel any pending resume context timer to prevent goroutine leak.
	mgr.resumeCancelMu.Lock()
	if mgr.resumeCancel != nil {
		mgr.resumeCancel()
	}
	mgr.resumeCancelMu.Unlock()

	// Step 3: Release the session lock — but ONLY if we still hold it.
	// If the lock was lost (detected by runLockRenewal), another worker may have
	// already acquired a new lock at the same Redis key. Calling ReleaseSession
	// would delete that worker's lock, causing a split-brain scenario.
	//
	// IMPORTANT: Lock release happens BEFORE any claim/requeue to prevent an
	// infinite loop where the same manager claims and requeues the same work
	// while still holding the lock.
	if mgr.lockLost.Load() {
		mgr.log(cleanupCtx, LogLevelInfo, "session_manager.skip_release_lock_lost", map[string]any{
			"session_id": mgr.sessionID,
			"message":    "lock was lost to another worker, skipping ReleaseSession to avoid deleting their lock",
		})
		cleanupSpan.SetAttributes(attribute.String("lock.release", "skipped"))
	} else {
		// Use a timeout context to prevent cleanup from blocking
		// indefinitely if Redis is unresponsive.
		cleanupSpan.AddEvent("release_session")
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer releaseCancel()
		if err := mgr.queue.ReleaseSession(releaseCtx, mgr.sessionID); err != nil {
			cleanupSpan.RecordError(err)
			mgr.log(cleanupCtx, LogLevelWarn, "session_manager.release_session_failed", map[string]any{
				"session_id": mgr.sessionID,
				"error":      err.Error(),
			})
		}
	}

	// Step 4: Notify workers about pending work.
	// After releasing the lock, any pending work in the queue can now be claimed
	// by a new manager. We publish a session:new notification so idle workers
	// wake up and create a new manager to process the remaining work.
	//
	// NOTE: We do NOT claim-and-requeue here. Previously, claimRemainingWork()
	// would claim work with the existing credential and requeue it, but this
	// caused an infinite loop: claim (lock still held) → requeue (back to
	// pending) → claim again → ... because ReleaseSession was called AFTER the
	// claim loop, so the lock was never released.
	if !mgr.lockLost.Load() {
		// Use a detached context for notification: the caller's ctx may be
		// cancelled (which is what triggered cleanup). Redis operations need
		// a live context to succeed. Mirrors the ReleaseSession pattern above.
		cleanupSpan.AddEvent("notify_pending_work")
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer notifyCancel() // defer ensures cancel runs even if notifyPendingWork panics
		mgr.notifyPendingWork(notifyCtx)
	}

	// Step 5: Remove from registry.
	if mgr.registry != nil {
		mgr.registry.Remove(mgr.sessionID)
	}

	mgr.log(cleanupCtx, LogLevelInfo, "session_manager.cleanup_done", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    mgr.turnID,
	})
}

// notifyPendingWork checks if the session queue has pending work and, if so,
// publishes a session:new notification to wake up idle workers.
// Called after ReleaseSession so that the notified workers can claim the lock.
func (mgr *SessionTurnManager) notifyPendingWork(ctx context.Context) {
	notifyCtx, notifySpan := mgr.startSpanIfEnabled(ctx, "notify_pending_work",
		trace.WithAttributes(
			attribute.String("session.id", mgr.sessionID),
		),
	)
	defer notifySpan.End()

	// Use the exported key accessor to avoid duplicating the Redis key format.
	queueKey := rtcqueue.SessionQueueKey(mgr.sessionID)
	count, err := mgr.queue.Client().ZCard(notifyCtx, queueKey).Result()
	if err != nil {
		// Log Redis failures so operators can detect connectivity issues.
		// Without this, silent failures leave pending work unnotified with no trace.
		notifySpan.RecordError(err)
		mgr.log(notifyCtx, LogLevelWarn, "session_manager.notify_pending_work_zcard_failed", map[string]any{
			"session_id": mgr.sessionID,
			"error":      err.Error(),
		})
		return
	}
	notifySpan.SetAttributes(attribute.Int("pending.count", int(count)))
	if count == 0 {
		notifySpan.SetAttributes(attribute.String("notify.status", "no_pending_work"))
		return
	}
	// Use the exported channel constant to avoid duplicating the channel name.
	if pubErr := mgr.queue.Client().Publish(notifyCtx, rtcqueue.ChannelSessionNew, mgr.sessionID).Err(); pubErr != nil {
		notifySpan.RecordError(pubErr)
		mgr.log(notifyCtx, LogLevelWarn, "session_manager.notify_pending_work_publish_failed", map[string]any{
			"session_id":    mgr.sessionID,
			"pending_count": count,
			"error":         pubErr.Error(),
		})
		return
	}
	notifySpan.SetAttributes(attribute.String("notify.status", "success"))
	mgr.log(notifyCtx, LogLevelInfo, "session_manager.notified_pending_work", map[string]any{
		"session_id":    mgr.sessionID,
		"pending_count": count,
	})
}

// runLockRenewal periodically renews the session lock TTL.
func (mgr *SessionTurnManager) runLockRenewal(ctx context.Context) {
	ticker := time.NewTicker(sessionRenewalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := mgr.queue.RenewLockWithCredential(ctx, mgr.sessionID, mgr.workerID, mgr.getCredential())
			if err != nil {
				// Transient Redis error: increment the failure counter but do not
				// immediately mark lockLost. Only N consecutive failures indicate
				// a genuine lock loss (guards against network jitter false positives).
				mgr.consecutiveRenewFailures.Add(1)
				failures := mgr.consecutiveRenewFailures.Load()
				mgr.log(ctx, LogLevelWarn, "session_manager.renewal_redis_error_transient", map[string]any{
					"session_id":           mgr.sessionID,
					"consecutive_failures": failures,
					"error":                err.Error(),
				})
				if failures >= rtcqueue.DefaultMaxConsecutiveRenewFailures {
					mgr.log(ctx, LogLevelError, "session_manager.renewal_giving_up", map[string]any{
						"session_id":           mgr.sessionID,
						"consecutive_failures": failures,
					})
					mgr.lockLost.Store(true)
					mgr.loop.Stop(adk.WithSkipCheckpoint())
					return
				}
				continue // Continue to the next renewal attempt
			}
			// Redis responded successfully — reset the failure counter.
			mgr.consecutiveRenewFailures.Store(0)
			if !ok {
				// Definite lock loss (preempted by another worker or TTL expired).
				mgr.log(ctx, LogLevelWarn, "session_manager.lock_lost", map[string]any{
					"session_id": mgr.sessionID,
				})
				mgr.lockLost.Store(true)
				mgr.loop.Stop(adk.WithSkipCheckpoint())
				return
			}
		}
	}
}

// log calls the optional log function if set.
func (mgr *SessionTurnManager) log(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
	if mgr.logFn != nil {
		mgr.logFn(ctx, level, msg, fields)
	}
}
