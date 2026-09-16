package turnagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
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

// maxConsecutiveRenewFailures is the threshold for consecutive Redis errors
// during lock renewal before the session is considered lost. Transient errors
// (network blips, connection pool exhaustion) are tolerated up to this count.
const maxConsecutiveRenewFailures = 3

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

	// consecutiveRenewFailures counts consecutive Redis errors during lock renewal.
	// Only when this reaches maxConsecutiveRenewFailures is lockLost set to true.
	// Transient errors (network blips, connection pool exhaustion) are tolerated
	// up to the threshold; only ok==false (definite lock loss) triggers immediately.
	consecutiveRenewFailures atomic.Int64

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
	go func() {
		defer func() {
			if r := recover(); r != nil {
				mgr.log(context.Background(), LogLevelError, "session_manager.run_lock_renewal_panic", map[string]any{
					"session_id": mgr.sessionID,
					"panic":      fmt.Sprintf("%v", r),
					"stack":      string(debug.Stack()),
				})
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
		// 使用带超时的 context 防止 Redis 挂起导致 goroutine 永不退出
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
		// 使用带超时的 context 防止 Redis 挂起导致 cleanup 阻塞
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer releaseCancel()
		if err := mgr.queue.ReleaseSession(releaseCtx, mgr.sessionID); err != nil {
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
	// Use the exported key accessor to avoid duplicating the Redis key format.
	queueKey := rtcqueue.SessionQueueKey(mgr.sessionID)
	count, err := mgr.queue.Client().ZCard(ctx, queueKey).Result()
	if err != nil || count == 0 {
		return
	}
	// Use the exported channel constant to avoid duplicating the channel name.
	mgr.queue.Client().Publish(ctx, rtcqueue.ChannelSessionNew, mgr.sessionID)
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
				// 瞬态 Redis 错误：仅递增失败计数，不立即标记 lockLost。
				// 连续 N 次失败才认为锁真正丢失（防止网络抖动导致 session 误判）。
				mgr.consecutiveRenewFailures.Add(1)
				failures := mgr.consecutiveRenewFailures.Load()
				mgr.log(ctx, LogLevelWarn, "session_manager.renewal_redis_error_transient", map[string]any{
					"session_id":           mgr.sessionID,
					"consecutive_failures": failures,
					"error":                err.Error(),
				})
				if failures >= maxConsecutiveRenewFailures {
					mgr.log(ctx, LogLevelError, "session_manager.renewal_giving_up", map[string]any{
						"session_id":           mgr.sessionID,
						"consecutive_failures": failures,
					})
					mgr.lockLost.Store(true)
					mgr.loop.Stop(adk.WithSkipCheckpoint())
					return
				}
				continue // 继续下次 renewal 尝试
			}
			// Redis 成功响应，重置失败计数
			mgr.consecutiveRenewFailures.Store(0)
			if !ok {
				// 明确的锁丢失（被其他 worker 抢占或 TTL 过期）
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
		GenInput:       mgr.genInput,
		GenResume:      mgr.genResume(&prevResumeCancel),
		PrepareAgent:   mgr.prepareAgent,
		OnAgentEvents:  mgr.onAgentEvents,
		Store:          mgr.cfg.CheckpointStore,
		CheckpointID:   mgr.checkpointID,
	}
}

// genInput builds the initial input for a fresh turn by loading messages from
// the database and converting them to eino format.
func (mgr *SessionTurnManager) genInput(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	items []TurnWorkItem,
) (*adk.GenInputResult[TurnWorkItem, *schema.Message], error) {
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
}

// genResume returns a GenResume callback that captures prevResumeCancel by
// pointer so it can cancel the previous resume context on each new call.
func (mgr *SessionTurnManager) genResume(prevResumeCancel *context.CancelFunc) func(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	interrupted, unhandled, newItems []TurnWorkItem,
) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
	return func(
		ctx context.Context,
		loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
		interrupted, unhandled, newItems []TurnWorkItem,
	) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
		return mgr.genResumeImpl(ctx, interrupted, unhandled, newItems, prevResumeCancel)
	}
}

// genResumeImpl builds the resume input for a checkpoint recovery.
func (mgr *SessionTurnManager) genResumeImpl(
	ctx context.Context,
	interrupted, unhandled, newItems []TurnWorkItem,
	prevResumeCancel *context.CancelFunc,
) (*adk.GenResumeResult[TurnWorkItem, *schema.Message], error) {
	// Cancel the previous resume context to release its timer.
	if *prevResumeCancel != nil {
		(*prevResumeCancel)()
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
	resumeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	*prevResumeCancel = cancel

	mgr.log(resumeCtx, LogLevelInfo, "gen_resume.called", map[string]any{
		"session_id":        mgr.sessionID,
		"turn_id":           turnID,
		"checkpoint_id":     mgr.checkpointID,
		"interrupted_count": len(interrupted),
		"unhandled_count":   len(unhandled),
		"newItems_count":    len(newItems),
	})

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
	// With batch resume: BatchResumeItems takes precedence over single fields.
	resumeParams := mgr.buildResumeParams(resumeCtx, newItems, turnID)

	if resumeParams == nil {
		mgr.log(resumeCtx, LogLevelWarn, "gen_resume.no_resume_params", map[string]any{
			"session_id":     mgr.sessionID,
			"turn_id":        turnID,
			"message":        "No InterruptID found in newItems - ResumeParams will be nil",
			"newItems_count": len(newItems),
		})
	}

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
}

// buildResumeParams constructs ResumeParams from newItems, handling both
// batch resume and single interrupt cases.
func (mgr *SessionTurnManager) buildResumeParams(
	ctx context.Context,
	newItems []TurnWorkItem,
	turnID string,
) *adk.ResumeParams {
	var resumeParams *adk.ResumeParams
	for _, item := range newItems {
		// Check for batch resume items first
		if len(item.BatchResumeItems) > 0 {
			if resumeParams == nil {
				resumeParams = &adk.ResumeParams{Targets: map[string]any{}}
			}
			for _, batchItem := range item.BatchResumeItems {
				resumeParams.Targets[batchItem.InterruptID] = batchItem.Result
				mgr.log(ctx, LogLevelInfo, "gen_resume.batch_resume_item", map[string]any{
					"session_id":   mgr.sessionID,
					"turn_id":      turnID,
					"interrupt_id": batchItem.InterruptID,
				})
			}
			mgr.log(ctx, LogLevelInfo, "gen_resume.batch_resume_complete", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
				"item_count": len(item.BatchResumeItems),
			})
			continue
		}

		// Fall back to single interrupt
		if item.InterruptID != "" {
			if resumeParams == nil {
				resumeParams = &adk.ResumeParams{Targets: map[string]any{}}
			}
			var result any
			if item.InterruptResult != nil {
				result = *item.InterruptResult
			} else if item.SubAgentResult != nil {
				result = *item.SubAgentResult
			}
			resumeParams.Targets[item.InterruptID] = result
			mgr.log(ctx, LogLevelInfo, "gen_resume.resume_params", map[string]any{
				"session_id":   mgr.sessionID,
				"turn_id":      turnID,
				"interrupt_id": item.InterruptID,
				"has_result":   result != nil,
			})
		}
	}
	return resumeParams
}

// prepareAgent creates the eino Agent with tools for the current turn.
func (mgr *SessionTurnManager) prepareAgent(
	ctx context.Context,
	loop *adk.TurnLoop[TurnWorkItem, *schema.Message],
	consumed []TurnWorkItem,
) (adk.Agent, error) {
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
}

// onAgentEvents consumes agent events, dispatching them to the appropriate
// handlers (stream, message, interrupt, error).
func (mgr *SessionTurnManager) onAgentEvents(
	ctx context.Context,
	tc *adk.TurnContext[TurnWorkItem, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent],
) error {
	turnID := TurnIDFromContext(ctx)
	mgr.log(ctx, LogLevelInfo, "on_agent_events.start", map[string]any{
		"session_id": mgr.sessionID,
		"turn_id":    turnID,
	})

	// Ensure CompleteWork is called even if we return early due to interrupt/error.
	completeWorkCalled := false
	completeWork := func() {
		if completeWorkCalled {
			return
		}
		completeWorkCalled = true
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bgCancel()
		for _, item := range tc.Consumed {
			if item.WorkID == "" {
				continue
			}
			if err := mgr.queue.CompleteWork(bgCtx, item.WorkID); err != nil {
				mgr.log(bgCtx, LogLevelError, "on_agent_events.complete_work_failed", map[string]any{
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

	// Idle warning watcher (warning-only, no exit).
	// AsyncIterator does not support Close()/Cancel(), so we only
	// log a warning when no events arrive for a prolonged period.
	activityReceived := make(chan struct{}, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				mgr.log(context.Background(), LogLevelError, "on_agent_events.idle_watcher_panic", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"panic":      fmt.Sprintf("%v", r),
				})
			}
		}()
		timer := time.NewTimer(eventIdleWarningTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activityReceived:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(eventIdleWarningTimeout)
			case <-timer.C:
				mgr.log(ctx, LogLevelError, "on_agent_events.idle_warning", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
					"timeout":    eventIdleWarningTimeout.String(),
					"action":     "warning_only_no_exit",
				})
			}
		}
	}()

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
			return nil
		}
		// Signal the idle watcher that we received an event.
		select {
		case activityReceived <- struct{}{}:
		default:
		}
		if err := mgr.dispatchEvents(ctx, turnID, ev); err != nil {
			var interruptErr *adk.InterruptError
			if errors.As(err, &interruptErr) {
				mgr.log(ctx, LogLevelInfo, "on_agent_events.interrupted", map[string]any{
					"session_id":   mgr.sessionID,
					"turn_id":      turnID,
					"num_contexts": len(interruptErr.InterruptContexts),
				})
				return err
			}
			mgr.log(ctx, LogLevelError, "on_agent_events.dispatch_error", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
				"error":      err.Error(),
			})
			return fmt.Errorf("turnagent: PublishEvent: %w", err)
		}
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

// StreamIdleTimeout is the maximum duration a stream can remain idle
// (no data received) before it is considered stalled and closed.
// This prevents turns from getting stuck indefinitely when the LLM
// stream stops mid-response.
const StreamIdleTimeout = 3 * time.Minute

// eventIdleWarningTimeout is the duration after which an idle warning is
// logged if no events are received from the AsyncIterator. AsyncIterator
// does not support Close()/Cancel(), so this is warning-only (方案 C).
const eventIdleWarningTimeout = 10 * time.Minute

// consumeStream drives a stream reader to completion.
// The stream is always closed when the function returns, regardless of the exit path.
func (mgr *SessionTurnManager) consumeStream(ctx context.Context, turnID, agentName, role, toolName string, stream *schema.StreamReader[*schema.Message]) error {
	defer stream.Close() // Single point of cleanup — prevents resource leaks if new exit paths are added.

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

	// Accumulate streamed content for lastMessage tracking (Sub Agent support)
	var streamedContent strings.Builder
	var streamedReasoningContent strings.Builder

	for {
		res, timedOut := RecvWithTimeout(ctx, stream.Recv, StreamIdleTimeout)

		if timedOut {
			mgr.log(ctx, LogLevelError, "stream.idle_timeout", map[string]any{
				"session_id": mgr.sessionID,
				"turn_id":    turnID,
				"timeout":    StreamIdleTimeout.String(),
			})
			return &StreamIdleTimeoutError{
				SessionID: mgr.sessionID,
				TurnID:    turnID,
				Timeout:   StreamIdleTimeout,
			}
		}

		if res.Err != nil {
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				mgr.log(ctx, LogLevelInfo, "stream.ctx_cancelled", map[string]any{
					"session_id": mgr.sessionID,
					"turn_id":    turnID,
				})
				return res.Err
			}
			if errors.Is(res.Err, io.EOF) {
				// For assistant messages, set lastMessage from accumulated content
				// This is needed for Sub Agent support to report the final result
				if role == string(schema.Assistant) {
					content := streamedContent.String()
					reasoning := streamedReasoningContent.String()
					if content != "" || reasoning != "" {
						mgr.setLastMessage(&Message{
							Role:             role,
							Content:          content,
							ReasoningContent: reasoning,
						})
					}
				}
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
			var cancelErr *adk.CancelError
			if errors.As(res.Err, &cancelErr) {
				return nil
			}
			if pubErr := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
				Kind:      EventKindError,
				AgentName: agentName,
				Err:       res.Err,
			}); pubErr != nil {
				return pubErr
			}
			return res.Err
		}

		var finishReason string
		var tokenUsage *TokenUsage
		if res.Msg.ResponseMeta != nil {
			finishReason = res.Msg.ResponseMeta.FinishReason
			updateMaxUsage(res.Msg.ResponseMeta.Usage)
			if res.Msg.ResponseMeta.Usage != nil {
				tokenUsage = extractTokenUsage(res.Msg.ResponseMeta.Usage)
			}
		}
		// Accumulate content for lastMessage tracking
		if res.Msg.Content != "" {
			streamedContent.WriteString(res.Msg.Content)
		}
		if res.Msg.ReasoningContent != "" {
			streamedReasoningContent.WriteString(res.Msg.ReasoningContent)
		}
		if err := mgr.cfg.PublishEvent(ctx, mgr.sessionID, turnID, &Event{
			Kind:             EventKindStreamChunk,
			AgentName:        agentName,
			Role:             role,
			ToolName:         toolName,
			Content:          res.Msg.Content,
			ReasoningContent: res.Msg.ReasoningContent,
			FinishReason:     finishReason,
			TokenUsage:       tokenUsage,
		}); err != nil {
			return err
		}
	}
}

// log calls the optional log function if set.
func (mgr *SessionTurnManager) log(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
	if mgr.logFn != nil {
		mgr.logFn(ctx, level, msg, fields)
	}
}
