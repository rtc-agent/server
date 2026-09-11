package turnagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
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

	renewCancel context.CancelFunc
	done        chan struct{} // closed when cleanup completes
	cleanupOnce sync.Once     // ensures cleanup runs exactly once

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

	// checkpointID is the eino checkpoint key for this session.
	checkpointID string
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
	mgr.renewCancel = cancel

	// Configure auto-exit: when the loop has been continuously idle for 100ms
	// (no items in buffer, blocking between turns), it exits automatically.
	// This must be set BEFORE Run because Run is blocking.
	mgr.loop.Stop(adk.UntilIdleFor(100 * time.Millisecond))

	// Start lock renewal goroutine.
	go mgr.runLockRenewal(renewCtx)

	// Monitor goroutine: when the loop exits, automatically run cleanup.
	// This ensures cleanup happens even if nobody calls Wait().
	// Use context.Background() for cleanup so that a cancelled parent context
	// does not prevent critical cleanup operations (ReleaseSession, requeue).
	// Use mgr.Wait() (not mgr.loop.Wait()) so the sync.Once protection applies.
	go func() {
		mgr.Wait()
		mgr.Cleanup(context.Background())
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

	// Step 1: Complete all pending work in the tracker (unblock waiting Process calls).
	mgr.tracker.CompleteAll()

	// Step 2: Stop lock renewal.
	if mgr.renewCancel != nil {
		mgr.renewCancel()
	}

	// Step 3: Release the session lock — but ONLY if we still hold it.
	// If the lock was lost (detected by runLockRenewal), another worker may have
	// already acquired a new lock at the same Redis key. Calling ReleaseSession
	// would delete that worker's lock, causing a split-brain scenario.
	//
	// IMPORTANT: Lock release happens BEFORE any claim/requeue to prevent an
	// infinite loop where the same manager claims and requeues the same work
	// while still holding the lock.
	if mgr.lockLost.Load() {
		mgr.log(ctx, LogLevelInfo, "session_manager.skip_release_lock_lost", map[string]any{
			"session_id": mgr.sessionID,
			"message":    "lock was lost to another worker, skipping ReleaseSession to avoid deleting their lock",
		})
	} else {
		if err := mgr.queue.ReleaseSession(context.Background(), mgr.sessionID); err != nil {
			mgr.log(ctx, LogLevelWarn, "session_manager.release_session_failed", map[string]any{
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
		mgr.notifyPendingWork(ctx)
	}

	// Step 5: Remove from registry.
	if mgr.registry != nil {
		mgr.registry.Remove(mgr.sessionID)
	}

	mgr.log(ctx, LogLevelInfo, "session_manager.cleanup_done", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    mgr.turnID,
	})
}

// notifyPendingWork checks if the session queue has pending work and, if so,
// publishes a session:new notification to wake up idle workers.
// Called after ReleaseSession so that the notified workers can claim the lock.
func (mgr *SessionTurnManager) notifyPendingWork(ctx context.Context) {
	queueKey := "queue:session:" + mgr.sessionID
	count, err := mgr.queue.Client().ZCard(ctx, queueKey).Result()
	if err != nil || count == 0 {
		return
	}
	// Publish notification so workers wake up and claim the pending work.
	mgr.queue.Client().Publish(ctx, "session:new", mgr.sessionID)
	mgr.log(ctx, LogLevelInfo, "session_manager.notified_pending_work", map[string]any{
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
				mgr.log(ctx, LogLevelError, "session_manager.renewal_redis_error", map[string]any{
					"session_id": mgr.sessionID,
					"error":      err.Error(),
				})
				mgr.lockLost.Store(true)
				mgr.loop.Stop(adk.WithSkipCheckpoint())
				return
			}
			if !ok {
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

// buildEinoConfig constructs the eino TurnLoopConfig for this manager.
func (mgr *SessionTurnManager) buildEinoConfig() adk.TurnLoopConfig[TurnWorkItem, *schema.Message] {
	// prevResumeCancel tracks the cancel function from the most recent GenResume call.
	// Each GenResume creates a fresh context.WithTimeout; the previous one must be
	// cancelled to release its timer resources.
	var prevResumeCancel context.CancelFunc

	return adk.TurnLoopConfig[TurnWorkItem, *schema.Message]{
		GenInput: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], items []TurnWorkItem) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
			if len(items) == 0 {
				return nil, fmt.Errorf("turnagent: GenInput called with no items")
			}
			turnID := items[0].TurnID

			// Diagnostic: check if this is a resume work item that ended up in GenInput
			// (which means checkpoint was NOT found, so TurnLoop fell back to GenInput)
			var isResumeWork bool
			var resumeInterruptID string
			for _, item := range items {
				if item.Kind == WorkKindResume {
					isResumeWork = true
					resumeInterruptID = item.InterruptID
					break
				}
			}
			if isResumeWork {
				mgr.log(ctx, LogLevelWarn, "gen_input.resume_work_fallback", map[string]any{
					"session_id":    mgr.sessionID,
					"turn_id":       turnID,
					"interrupt_id":  resumeInterruptID,
					"message":       "Resume work item in GenInput - checkpoint was NOT found, starting fresh turn",
					"checkpoint_id": mgr.checkpointID,
				})
			}

			mgr.log(ctx, LogLevelInfo, "gen_input.start", map[string]any{
				"session_id":    mgr.sessionID,
				"turn_id":       turnID,
				"item_count":    len(items),
				"is_resume":     isResumeWork,
				"checkpoint_id": mgr.checkpointID,
			})

			ctx = WithSessionID(ctx, mgr.sessionID)
			ctx = WithTurnID(ctx, turnID)

			if len(mgr.cfg.Callbacks) > 0 {
				ctx = callbacks.InitCallbacks(ctx, &callbacks.RunInfo{}, mgr.cfg.Callbacks...)
			}

			msgs, err := mgr.cfg.LoadMessages(ctx, mgr.sessionID)
			if err != nil {
				return nil, fmt.Errorf("turnagent: LoadMessages: %w", err)
			}
			if len(msgs) == 0 {
				mgr.log(ctx, LogLevelWarn, "turn.empty_messages", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
				})
			}
			return &adk.GenInputResult[TurnWorkItem, *schema.Message]{
				RunCtx: ctx,
				Input: &adk.TypedAgentInput[*schema.Message]{
					Messages:        toEinoMessages(msgs),
					EnableStreaming: true,
				},
				Consumed: items,
			}, nil
		},

		GenResume: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], interrupted, unhandled, newItems []TurnWorkItem) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
			// Cancel the previous resume context to release its timer.
			// Each GenResume creates a fresh context; the previous one is no longer needed.
			if prevResumeCancel != nil {
				prevResumeCancel()
			}

			var turnID string
			if len(newItems) > 0 {
				turnID = newItems[0].TurnID
			} else if len(interrupted) > 0 {
				turnID = interrupted[0].TurnID
			} else if len(unhandled) > 0 {
				turnID = unhandled[0].TurnID
			}

			// Create a fresh context with timeout for this resume attempt.
			// This prevents context cancellation issues when work items are
			// requeued and processed by different workers after interrupt/resume cycles.
			resumeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			prevResumeCancel = cancel // Tracked for cleanup on next GenResume call

			// Diagnostic: log all items received by GenResume
			mgr.log(resumeCtx, LogLevelInfo, "gen_resume.called", map[string]any{
				"session_id":        mgr.sessionID,
				"turn_id":           turnID,
				"checkpoint_id":     mgr.checkpointID,
				"interrupted_count": len(interrupted),
				"unhandled_count":   len(unhandled),
				"newItems_count":    len(newItems),
			})

			// Log details of newItems (these should contain the resume work item)
			for i, item := range newItems {
				mgr.log(resumeCtx, LogLevelDebug, "gen_resume.newItem", map[string]any{
					"index":            i,
					"kind":             item.Kind,
					"turn_id":          item.TurnID,
					"work_id":          item.WorkID,
					"interrupt_id":     item.InterruptID,
					"has_result":       item.InterruptResult != nil,
					"sub_agent_result": item.SubAgentResult != nil,
				})
			}

			resumeCtx = WithSessionID(resumeCtx, mgr.sessionID)
			if turnID != "" {
				resumeCtx = WithTurnID(resumeCtx, turnID)
			}

			if len(mgr.cfg.Callbacks) > 0 {
				resumeCtx = callbacks.InitCallbacks(resumeCtx, &callbacks.RunInfo{}, mgr.cfg.Callbacks...)
			}

			allItems := make([]TurnWorkItem, 0, len(interrupted)+len(unhandled)+len(newItems))
			allItems = append(allItems, interrupted...)
			allItems = append(allItems, unhandled...)
			allItems = append(allItems, newItems...)

			// Build ResumeParams from the resume item's interrupt fields.
			// This follows the pattern in eino's official example:
			// the application persists the interrupt ID, includes it in the
			// resume work payload along with the result, and GenResume builds
			// ResumeParams.Targets so eino can resume the interrupted tool.
			//
			// With batch resume: we now support multiple interrupts per turn.
			// BatchResumeItems contains all interrupt IDs and their results.
			// If BatchResumeItems is present, it takes precedence over the
			// single InterruptID/InterruptResult fields.
			var resumeParams *adk.ResumeParams
			for _, item := range newItems {
				// Check for batch resume items first
				if len(item.BatchResumeItems) > 0 {
					if resumeParams == nil {
						resumeParams = &adk.ResumeParams{
							Targets: map[string]any{},
						}
					}
					for _, batchItem := range item.BatchResumeItems {
						resumeParams.Targets[batchItem.InterruptID] = batchItem.Result
						mgr.log(resumeCtx, LogLevelInfo, "gen_resume.batch_resume_item", map[string]any{
							"session_id":   mgr.sessionID,
							"turn_id":      turnID,
							"interrupt_id": batchItem.InterruptID,
						})
					}
					mgr.log(resumeCtx, LogLevelInfo, "gen_resume.batch_resume_complete", map[string]any{
						"session_id": mgr.sessionID,
						"turn_id":    turnID,
						"item_count": len(item.BatchResumeItems),
					})
					continue
				}

				// Fall back to single interrupt
				if item.InterruptID != "" {
					if resumeParams == nil {
						resumeParams = &adk.ResumeParams{
							Targets: map[string]any{},
						}
					}
					// Use InterruptResult if set, otherwise fall back to SubAgentResult.
					var result any
					if item.InterruptResult != nil {
						result = *item.InterruptResult
					} else if item.SubAgentResult != nil {
						result = *item.SubAgentResult
					}
					resumeParams.Targets[item.InterruptID] = result

					mgr.log(resumeCtx, LogLevelInfo, "gen_resume.resume_params", map[string]any{
						"session_id":   mgr.sessionID,
						"turn_id":      turnID,
						"interrupt_id": item.InterruptID,
						"has_result":   result != nil,
					})
				}
			}

			// Diagnostic: log if ResumeParams was NOT built
			if resumeParams == nil {
				mgr.log(resumeCtx, LogLevelWarn, "gen_resume.no_resume_params", map[string]any{
					"session_id":     mgr.sessionID,
					"turn_id":        turnID,
					"message":        "No InterruptID found in newItems - ResumeParams will be nil",
					"newItems_count": len(newItems),
				})
			}

			// Debug logging: record GenResume result for troubleshooting.
			mgr.log(resumeCtx, LogLevelInfo, "gen_resume.result", map[string]any{
				"session_id":        mgr.sessionID,
				"turn_id":           turnID,
				"consumed_count":    len(allItems),
				"has_resume_params": resumeParams != nil,
				"interrupted_count": len(interrupted),
				"unhandled_count":   len(unhandled),
				"newItems_count":    len(newItems),
			})

			return &adk.GenResumeResult[TurnWorkItem, *schema.Message]{
				RunCtx:       resumeCtx,
				Consumed:     allItems,
				ResumeParams: resumeParams,
			}, nil
		},

		PrepareAgent: func(ctx context.Context, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], consumed []TurnWorkItem) (adk.Agent, error) {
			var turnID string
			if len(consumed) > 0 {
				turnID = consumed[0].TurnID
			}
			if turnID == "" {
				turnID = TurnIDFromContext(ctx)
			}

			mgr.log(ctx, LogLevelDebug, "prepare_agent.start", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
			})

			tools, err := mgr.cfg.CreateTools(ctx, mgr.sessionID, turnID)
			if err != nil {
				return nil, fmt.Errorf("turnagent: CreateTools: %w", err)
			}

			agent, err := mgr.cfg.CreateAgent(ctx, mgr.sessionID, turnID, tools)
			if err != nil {
				return nil, fmt.Errorf("turnagent: CreateAgent: %w", err)
			}
			return agent, nil
		},

		OnAgentEvents: func(ctx context.Context, tc *adk.TurnContext[TurnWorkItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			turnID := TurnIDFromContext(ctx)
			mgr.log(ctx, LogLevelInfo, "on_agent_events.start", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
			})

			// Ensure CompleteWork is called even if we return early due to interrupt/error.
			// This prevents work items from being stuck in "processing" state.
			completeWorkCalled := false
			completeWork := func() {
				if completeWorkCalled {
					return
				}
				completeWorkCalled = true
				bgCtx := context.Background()
				for _, item := range tc.Consumed {
					if item.WorkID == "" {
						continue
					}
					if err := mgr.queue.CompleteWork(bgCtx, item.WorkID); err != nil {
						mgr.log(ctx, LogLevelError, "on_agent_events.complete_work_failed", map[string]any{
							"session_id": mgr.sessionID,
							"turn_id":    turnID,
							"work_id":    item.WorkID,
							"error":      err.Error(),
						})
					}
					mgr.tracker.Complete(item.WorkID)
				}
			}
			defer completeWork()

			for {
				ctxErr := ctx.Err()
				mgr.log(ctx, LogLevelDebug, "on_agent_events.waiting_next", map[string]any{
					"session_id":  mgr.sessionID,
					"turn_id":     turnID,
					"context_err": ctxErr,
				})
				ev, ok := events.Next()
				if !ok {
					mgr.log(ctx, LogLevelInfo, "on_agent_events.done", map[string]any{
						"session_id": mgr.sessionID,
						"turn_id":    turnID,
					})
					// CompleteWork will be called by defer
					return nil
				}
				if err := mgr.dispatchEvents(ctx, turnID, ev); err != nil {
					var interruptErr *adk.InterruptError
					if errors.As(err, &interruptErr) {
						mgr.log(ctx, LogLevelInfo, "on_agent_events.interrupted", map[string]any{
							"session_id":   mgr.sessionID,
							"turn_id":      turnID,
							"num_contexts": len(interruptErr.InterruptContexts),
						})
						// CompleteWork will be called by defer
						return err
					}
					mgr.log(ctx, LogLevelError, "on_agent_events.dispatch_error", map[string]any{
						"session_id": mgr.sessionID,
						"turn_id":    turnID,
						"error":      err.Error(),
					})
					// CompleteWork will be called by defer
					return fmt.Errorf("turnagent: PublishEvent: %w", err)
				}
			}
		},

		Store:        mgr.cfg.CheckpointStore,
		CheckpointID: mgr.checkpointID,
	}
}

// dispatchEvents translates one eino AgentEvent into flattened Event calls.
func (mgr *SessionTurnManager) dispatchEvents(ctx context.Context, turnID string, ev *adk.AgentEvent) error {
	if ev.Err != nil {
		var cancelErr *adk.CancelError
		if errors.As(ev.Err, &cancelErr) {
			return nil
		}
		mgr.log(ctx, LogLevelError, "event.error", map[string]any{
			"session_id": mgr.sessionID,
			"turn_id":    turnID,
			"agent_name": ev.AgentName,
			"error":      ev.Err.Error(),
		})
		return mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
			Kind:      EventKindError,
			AgentName: ev.AgentName,
			Err:       ev.Err,
		})
	}

	if ev.Output == nil || ev.Output.MessageOutput == nil {
		if ev.Action != nil && ev.Action.Interrupted != nil {
			return &adk.InterruptError{
				InterruptContexts: ev.Action.Interrupted.InterruptContexts,
			}
		}
		return nil
	}
	mv := ev.Output.MessageOutput

	if mv.IsStreaming {
		return mgr.consumeStream(ctx, turnID, ev.AgentName, string(mv.Role), mv.ToolName, mv.MessageStream)
	}

	var tokenUsage *TokenUsage
	if mv.Message != nil && mv.Message.ResponseMeta != nil && mv.Message.ResponseMeta.Usage != nil {
		tokenUsage = extractTokenUsage(mv.Message.ResponseMeta.Usage)
	}
	if mv.Role == schema.Assistant && mv.Message != nil {
		mgr.setLastMessage(fromEinoMessage(mv.Message))
	}
	return mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
		Kind:       EventKindMessage,
		AgentName:  ev.AgentName,
		Role:       string(mv.Role),
		ToolName:   mv.ToolName,
		Message:    fromEinoMessage(mv.Message),
		TokenUsage: tokenUsage,
	})
}

// consumeStream drives a stream reader to completion.
func (mgr *SessionTurnManager) consumeStream(ctx context.Context, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message]) error {
	mgr.log(ctx, LogLevelDebug, "stream.consume_start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
		"agent_name": agentName,
		"role":       role,
	})

	var maxUsage *schema.TokenUsage
	updateMaxUsage := func(usage *schema.TokenUsage) {
		if usage == nil {
			return
		}
		if maxUsage == nil {
			maxUsage = &schema.TokenUsage{}
		}
		if usage.PromptTokens > maxUsage.PromptTokens {
			maxUsage.PromptTokens = usage.PromptTokens
		}
		if usage.CompletionTokens > maxUsage.CompletionTokens {
			maxUsage.CompletionTokens = usage.CompletionTokens
		}
		if usage.TotalTokens > maxUsage.TotalTokens {
			maxUsage.TotalTokens = usage.TotalTokens
		}
		if usage.PromptTokenDetails.CachedTokens > maxUsage.PromptTokenDetails.CachedTokens {
			maxUsage.PromptTokenDetails.CachedTokens = usage.PromptTokenDetails.CachedTokens
		}
		if usage.CompletionTokensDetails.ReasoningTokens > maxUsage.CompletionTokensDetails.ReasoningTokens {
			maxUsage.CompletionTokensDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
		}
	}

	type recvResult struct {
		msg *schema.Message
		err error
	}

	for {
		ch := make(chan recvResult, 1)
		go func() {
			msg, err := stream.Recv()
			ch <- recvResult{msg, err}
		}()

		select {
		case <-ctx.Done():
			mgr.log(ctx, LogLevelInfo, "stream.ctx_cancelled", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
			})
			stream.Close()
			return ctx.Err()

		case res := <-ch:
			if errors.Is(res.err, io.EOF) {
				stream.Close()
				var aggregatedTokenUsage *TokenUsage
				if maxUsage != nil {
					aggregatedTokenUsage = extractTokenUsage(maxUsage)
				}
				return mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
					Kind:       EventKindStreamEnd,
					AgentName:  agentName,
					Role:       role,
					ToolName:   toolName,
					TokenUsage: aggregatedTokenUsage,
				})
			}
			if res.err != nil {
				var cancelErr *adk.CancelError
				if errors.As(res.err, &cancelErr) {
					stream.Close()
					return nil
				}
				stream.Close()
				if pubErr := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
					Kind:      EventKindError,
					AgentName: agentName,
					Err:       res.err,
				}); pubErr != nil {
					return pubErr
				}
				return res.err
			}

			var finishReason string
			var tokenUsage *TokenUsage
			if res.msg.ResponseMeta != nil {
				finishReason = res.msg.ResponseMeta.FinishReason
				updateMaxUsage(res.msg.ResponseMeta.Usage)
				if res.msg.ResponseMeta.Usage != nil {
					tokenUsage = extractTokenUsage(res.msg.ResponseMeta.Usage)
				}
			}
			if err := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
				Kind:             EventKindStreamChunk,
				AgentName:        agentName,
				Role:             role,
				ToolName:         toolName,
				Content:          res.msg.Content,
				ReasoningContent: res.msg.ReasoningContent,
				FinishReason:     finishReason,
				TokenUsage:       tokenUsage,
			}); err != nil {
				stream.Close()
				return err
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
