package turnagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
)

// renewalInterval is how often the session lock is renewed.
// Must be well below DefaultLockTTLSeconds (120s) to prevent expiry.
const renewalInterval = 30 * time.Second

// TurnAgent manages the lifecycle of a single turn for a session.
//
// A TurnAgent is created per turn (not per session). It claims the session lock,
// runs a pump loop to pop work items from the queue and push them to the eino
// TurnLoop, and periodically renews the lock. When the turn ends (the loop
// exits), the TurnAgent stops the pump loop and releases the lock.
//
// Lifecycle:
//
//	ta, err := NewTurnAgent(ctx, queue, sessionID, workerID, credential, turnID, loop)
//	go ta.Run(ctx)
//	// ... loop runs ...
//	exitState := ta.Wait()
//	// or: ta.Stop() to abort the turn
type TurnAgent struct {
	sessionID  string
	workerID   string
	turnID     string // turnID for work items created by the pump loop
	queue      *rtcqueue.Queue

	credMu     sync.Mutex
	credential string // lock credential from initial ClaimWithCredential

	loop       *adk.TurnLoop[TurnWorkItem, *schema.Message]

	// logFn is an optional closure for structured logging. When non-nil, it
	// delegates to Agent.logIfEnabled, allowing TurnAgent to emit logs without
	// holding a direct reference to Agent (which would create a circular
	// dependency). Set via the variadic parameter of NewTurnAgent.
	logFn func(ctx context.Context, level LogLevel, msg string, fields map[string]any)

	pumpCancel context.CancelFunc
	pumpDone   chan struct{} // closed when pump goroutine exits
	renewDone  chan struct{} // closed when lock renewal goroutine exits

	initOnce sync.Once
	initDone chan struct{} // closed when Run() initialization completes

	stopOnce sync.Once
	stopped  chan struct{} // closed when cleanup completes

	// turnDone is signaled each time a turn completes (OnAgentEvents returns).
	// Process() waits on this channel to know when to check for next work.
	turnDone chan struct{}
}

// NewTurnAgent creates a TurnAgent with an existing lock credential.
//
// The caller (rtc-queue Worker) already holds the session lock and passes its
// credential so that the TurnAgent can perform subsequent claims and lock
// renewals without re-acquiring the lock. This avoids the race where the
// Worker has already claimed the lock but the queue is now empty, causing
// TurnAgent's independent claim to fail.
//
// credential must be non-empty. The TurnAgent does not claim the lock itself;
// it relies on the caller to provide a valid credential from an existing claim.
//
// turnID is the turn identifier for work items created by the pump loop. It
// is set at construction time because the pump loop may create additional
// work items (e.g., resume) after the initial work has been processed, and
// those items must carry the same turnID.
func NewTurnAgent(ctx context.Context, queue *rtcqueue.Queue, sessionID, workerID, credential, turnID string, loop *adk.TurnLoop[TurnWorkItem, *schema.Message], logFn ...func(ctx context.Context, level LogLevel, msg string, fields map[string]any)) (*TurnAgent, error) {
	if credential == "" {
		return nil, fmt.Errorf("turnagent: credential is required: TurnAgent must receive credential from Worker")
	}

	ta := &TurnAgent{
		sessionID:  sessionID,
		workerID:   workerID,
		turnID:     turnID,
		credential: credential,
		queue:      queue,
		loop:       loop,
		initDone:   make(chan struct{}),
		stopped:    make(chan struct{}),
		turnDone:   make(chan struct{}, 1), // buffered to avoid blocking
	}

	// Store the optional log function (first variadic argument, if provided).
	if len(logFn) > 0 {
		ta.logFn = logFn[0]
	}

	ta.log(ctx, LogLevelInfo, "turnagent.created", map[string]any{
		"session_id": sessionID,
		"turn_id":    turnID,
		"worker_id":  workerID,
	})

	return ta, nil
}

// Run starts the pump loop and lock renewal goroutines.
//
// The pump loop subscribes to session:new notifications, pops work items from
// the queue, and pushes them to the TurnLoop buffer. The lock renewal
// goroutine periodically refreshes the session lock TTL.
//
// A monitor goroutine is also started that auto-stops the TurnAgent when the
// loop exits (e.g., after a complete turn or an interrupt). This ensures the
// lock is released and resources are freed even when the caller does not
// explicitly call Stop.
//
// Run must be called at most once. The loop's context is derived from ctx,
// so cancelling ctx will propagate to the loop and trigger a graceful shutdown.
func (ta *TurnAgent) Run(ctx context.Context) {
	ta.initOnce.Do(func() {
		defer close(ta.initDone)

		pumpCtx, cancel := context.WithCancel(ctx)
		ta.pumpCancel = cancel
		ta.pumpDone = make(chan struct{})
		ta.renewDone = make(chan struct{})

		// Start pump loop: pop work items and push to TurnLoop buffer.
		go func() {
			defer close(ta.pumpDone)
			ta.runPumpLoop(pumpCtx)
		}()

		// Start lock renewal: periodically refresh the session lock TTL.
		go func() {
			defer close(ta.renewDone)
			ta.runLockRenewal(pumpCtx)
		}()

		// Monitor loop exit: when the TurnLoop exits (complete, interrupt, or
		// error), auto-stop the TurnAgent to release resources and the lock.
		go func() {
			ta.loop.Wait()
			ta.Stop()
		}()

		// Start the TurnLoop. Run is non-blocking; the loop runs in a goroutine
		// managed by eino. Wait() blocks until the loop exits.
		ta.loop.Run(ctx)
	})
}

// runPumpLoop subscribes to the session:new channel and processes notifications.
//
// For each notification matching this agent's session, it claims the next work
// item using the stored credential, loads the work payload, decodes it into a
// TurnWorkItem, and pushes it to the TurnLoop buffer.
//
// The goroutine exits when ctx is cancelled (during Stop or lock loss).
func (ta *TurnAgent) runPumpLoop(ctx context.Context) {
	sub := ta.queue.SubscribeNew(ctx)
	defer func() { _ = sub.Close() }()
	ch := sub.Channel()

	ta.log(ctx, LogLevelInfo, "pump.subscribed", map[string]any{
		"session_id": ta.sessionID,
		"turn_id":    ta.turnID,
	})

	for {
		select {
		case <-ctx.Done():
			ta.log(ctx, LogLevelInfo, "pump.ctx_done", map[string]any{
				"session_id": ta.sessionID,
				"turn_id":    ta.turnID,
				"reason":     ctx.Err().Error(),
			})
			return
		case msg, ok := <-ch:
			if !ok {
				ta.log(ctx, LogLevelInfo, "pump.channel_closed", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
				})
				return
			}
			// Filter: only process notifications for our session.
			if msg.Payload != ta.sessionID {
				ta.log(ctx, LogLevelDebug, "pump.notification_skipped", map[string]any{
					"session_id":       ta.sessionID,
					"turn_id":          ta.turnID,
					"notification_sid": msg.Payload,
				})
				continue
			}

			ta.log(ctx, LogLevelDebug, "pump.notification_received", map[string]any{
				"session_id": ta.sessionID,
				"turn_id":    ta.turnID,
			})

			// Guard: if ctx is already cancelled, exit before claiming.
			// This prevents a race where the pump loop picks up a notification
			// after the turn has ended, potentially claiming work with a stale
			// turnID and pushing it to the wrong TurnLoop.
			if ctx.Err() != nil {
				ta.log(ctx, LogLevelInfo, "pump.ctx_cancelled_before_claim", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
				})
				return
			}

			// Claim the next work item with our credential.
			// ClaimWithCredential returns nil (no error) when the queue is empty
			// or the credential is rejected (lock lost to another worker).
			claim, err := ta.queue.ClaimWithCredential(ctx, ta.sessionID, ta.workerID, ta.getCredential())
			if err != nil {
				ta.log(ctx, LogLevelWarn, "pump.claim_error", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
					"error":      err.Error(),
				})
				continue
			}
			if claim == nil {
				ta.log(ctx, LogLevelDebug, "pump.claim_empty", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
					"message":    "queue empty or credential rejected",
				})
				continue
			}

			ta.log(ctx, LogLevelDebug, "pump.claimed", map[string]any{
				"session_id": ta.sessionID,
				"turn_id":    ta.turnID,
				"work_id":    claim.WorkID,
			})

			// Update credential in case the queue rotated it.
			if claim.Credential != "" {
				ta.setCredential(claim.Credential)
			}

			// Load the full work item from Redis.
			work, err := ta.queue.LoadWork(ctx, claim.WorkID)
			if err != nil || work == nil {
				var errStr string
				if err != nil {
					errStr = err.Error()
				}
				ta.log(ctx, LogLevelWarn, "pump.load_work_failed", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
					"work_id":    claim.WorkID,
					"error":      errStr,
				})
				continue
			}

			// Decode the JSON payload into WorkPayload.
			var payload WorkPayload
			if err := json.Unmarshal([]byte(work.Data), &payload); err != nil {
				ta.log(ctx, LogLevelWarn, "pump.decode_failed", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
					"work_id":    claim.WorkID,
					"error":      err.Error(),
				})
				continue
			}

			// Push to TurnLoop buffer. Push returns false if the loop has stopped.
			item := TurnWorkItem{
				WorkPayload: payload,
				TurnID:      ta.turnID,
			}
			pushed, _ := ta.loop.Push(item)
			if !pushed {
				// Loop has stopped; exit the pump.
				ta.log(ctx, LogLevelInfo, "pump.loop_stopped", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
					"message":    "TurnLoop stopped, exiting pump",
				})
				return
			}

			ta.log(ctx, LogLevelDebug, "pump.pushed", map[string]any{
				"session_id": ta.sessionID,
				"turn_id":    ta.turnID,
				"work_id":    claim.WorkID,
				"work_kind":  string(payload.Kind),
			})

			// NOTE: work completion is now handled by Process() via
			// CompleteWorkAndClaimNext, not by the pump loop. The pump loop
			// only claims and pushes work items; the actual completion happens
			// after the LLM finishes processing the work.
		}
	}
}

// runLockRenewal periodically renews the session lock TTL.
//
// Every 30 seconds, it calls RenewLockWithCredential to extend the lock.
// If the renewal fails (lock lost to another worker, or Redis error), it
// tells eino to skip checkpoint saving (so another node does not see a stale
// checkpoint) and cancels the pump context to stop all activity.
func (ta *TurnAgent) runLockRenewal(ctx context.Context) {
	ticker := time.NewTicker(renewalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			ta.log(ctx, LogLevelInfo, "renewal.ctx_done", map[string]any{
				"session_id": ta.sessionID,
				"turn_id":    ta.turnID,
			})
			return
		case <-ticker.C:
			ok, err := ta.queue.RenewLockWithCredential(ctx, ta.sessionID, ta.workerID, ta.getCredential())
			if err != nil {
				// Redis error - cannot confirm lock validity.
				// Stop loop without checkpoint (another node may take over).
				ta.log(ctx, LogLevelError, "renewal.redis_error", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
					"error":      err.Error(),
				})
				ta.loop.Stop(adk.WithSkipCheckpoint())
				ta.pumpCancel()
				return
			}
			if !ok {
				// Lock lost (TTL expired, or another worker took over).
				// Tell eino not to persist a checkpoint — another node will
				// take over the session and must not see a stale checkpoint.
				ta.log(ctx, LogLevelWarn, "renewal.lock_lost", map[string]any{
					"session_id": ta.sessionID,
					"turn_id":    ta.turnID,
				})
				ta.loop.Stop(adk.WithSkipCheckpoint())
				ta.pumpCancel()
				return
			}
			ta.log(ctx, LogLevelDebug, "renewal.success", map[string]any{
				"session_id": ta.sessionID,
				"turn_id":    ta.turnID,
			})
		}
	}
}

// Stop initiates a graceful shutdown of the TurnAgent.
//
// The shutdown sequence is:
//  1. Wait for Run() initialization to complete (ensures safe field access)
//  2. Cancel pump loop context (stops popping work items)
//  3. Stop TurnLoop (signals the loop to finish the current turn and exit)
//  4. Wait for pump and lock renewal goroutines to exit
//  5. Wait for TurnLoop to exit
//  6. Release session lock from Redis
//
// Stop is safe to call multiple times; subsequent calls are no-ops.
// Run must be called before Stop; the internal monitor goroutine guarantees
// this by calling Stop after loop exit.
func (ta *TurnAgent) Stop() {
	// Wait for Run initialization to complete before accessing pumpCancel/pumpDone.
	// Run must be called before Stop; if Run hasn't been called this blocks until
	// it is (the monitor goroutine calls Stop after loop exit, which guarantees
	// Run has completed initialization).
	<-ta.initDone

	ta.stopOnce.Do(func() {
		defer close(ta.stopped)

		// Step 1: Stop pump loop.
		if ta.pumpCancel != nil {
			ta.pumpCancel()
		}

		// Step 2: Stop TurnLoop (graceful: let the current turn finish).
		if ta.loop != nil {
			ta.loop.Stop()
		}

		// Step 3: Wait for goroutines to exit.
		if ta.pumpDone != nil {
			<-ta.pumpDone
		}
		if ta.renewDone != nil {
			<-ta.renewDone
		}

		// Step 4: Wait for TurnLoop to exit (if it hasn't already).
		if ta.loop != nil {
			ta.loop.Wait()
		}

		// Step 5: Release the session lock.
		// Use a background context because the original context may be cancelled.
		_ = ta.queue.ReleaseSession(context.Background(), ta.sessionID)
	})
}

// Wait blocks until the TurnLoop exits and returns the exit state.
//
// The exit state contains the exit reason (nil for clean exit), unhandled
// items, interrupted items, and checkpoint information.
//
// If Run was not called, Wait blocks forever. Always call Run before Wait.
func (ta *TurnAgent) Wait() *adk.TurnLoopExitState[TurnWorkItem, *schema.Message] {
	if ta.loop == nil {
		// Run was not called; this is a programming error.
		// Return a synthetic exit state to prevent blocking forever.
		return &adk.TurnLoopExitState[TurnWorkItem, *schema.Message]{
			ExitReason: errors.New("turnagent: Wait called before Run"),
		}
	}
	return ta.loop.Wait()
}

// Done returns a channel that is closed when the TurnAgent has fully stopped.
// This includes the pump loop, lock renewal, TurnLoop, and lock release.
func (ta *TurnAgent) Done() <-chan struct{} {
	return ta.stopped
}

// SessionID returns the session this TurnAgent is managing.
func (ta *TurnAgent) SessionID() string {
	return ta.sessionID
}

// Credential returns the lock credential for this TurnAgent.
// It is used for subsequent ClaimWithCredential calls within the same turn.
func (ta *TurnAgent) Credential() string {
	return ta.getCredential()
}

func (ta *TurnAgent) setCredential(cred string) {
	ta.credMu.Lock()
	defer ta.credMu.Unlock()
	ta.credential = cred
}

// UpdateCredential updates the lock credential from an external caller (e.g.,
// Process() when CompleteWorkAndClaimNext returns a new credential). This is
// the public counterpart of setCredential, safe for cross-goroutine use.
func (ta *TurnAgent) UpdateCredential(cred string) {
	if cred == "" {
		return
	}
	ta.setCredential(cred)
}

func (ta *TurnAgent) getCredential() string {
	ta.credMu.Lock()
	defer ta.credMu.Unlock()
	return ta.credential
}

// log calls the optional log function if set. It is a no-op when logFn is nil
// (e.g., in tests that do not provide a logger).
func (ta *TurnAgent) log(ctx context.Context, level LogLevel, msg string, fields map[string]any) {
	if ta.logFn != nil {
		ta.logFn(ctx, level, msg, fields)
	}
}

// SignalTurnComplete signals that a turn has completed. Called by OnAgentEvents
// when the agent finishes producing events. This allows Process() to check for
// next work and either push it to the buffer or call Stop().
func (ta *TurnAgent) SignalTurnComplete() {
	select {
	case ta.turnDone <- struct{}{}:
	default:
		// Channel already has a signal, don't block
	}
}

// WaitForTurnComplete waits for a turn completion signal. Returns immediately
// if a signal is available, otherwise blocks until a turn completes or the
// TurnAgent is stopped.
func (ta *TurnAgent) WaitForTurnComplete(ctx context.Context) bool {
	select {
	case <-ta.turnDone:
		return true
	case <-ta.stopped:
		return false
	case <-ctx.Done():
		return false
	}
}
