package server

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
)

// recoverStaleTurns recovers stale turns after server restart.
//
// Startup recovery differs from the periodic runtime scanner (staleTurnScanner):
//   - No time thresholds — recovers ALL stale turns immediately.
//   - No Worker liveness or checkpoint checks (no Workers connected at startup).
//   - Publishes kind="resume" (preserving InterruptID) instead of kind="submit".
//   - Performs ghost-work cleanup and session-lock release (runtime scanner does not).
func (s *Server) recoverStaleTurns(ctx context.Context) {
	staleTurns, err := s.svcCtx.TurnRepo.FindStaleTurns(ctx, staleTurnStatuses)
	if err != nil {
		logger.Error(ctx, "[Server] recoverStaleTurns: find stale turns", zap.Error(err))
		return
	}

	if len(staleTurns) == 0 {
		return
	}

	logger.Info(ctx, "[Server] recoverStaleTurns: found stale turns",
		zap.Int("count", len(staleTurns)))

	// Collect unique session IDs for ghost work cleanup
	sessionIDs := make(map[string]bool)

	// sessionStatusCache avoids repeated DB queries for sessions with multiple
	// stale turns. Key: sessionID string, Value: session status string.
	sessionStatusCache := make(map[string]string)

	for _, turn := range staleTurns {
		sessionID := turn.SessionID.String()

		// Check if the session is closed. Resume work items for closed sessions
		// would be wasted — the worker would process them only to discover the
		// session is closed. Skip early to save resources.
		cachedStatus, cached := sessionStatusCache[sessionID]
		if !cached {
			session, sessErr := s.svcCtx.SessionRepo.GetByID(ctx, turn.SessionID)
			if sessErr != nil {
				logger.Warn(ctx, "[Server] recoverStaleTurns: load session failed",
					zap.String("turn_id", turn.ID.String()),
					zap.String("session_id", sessionID),
					zap.Error(sessErr))
			} else if session != nil {
				cachedStatus = session.Status
			}
			sessionStatusCache[sessionID] = cachedStatus
		}
		if cachedStatus == string(model.SessionStatusClosed) {
			logger.Info(ctx, "[Server] recoverStaleTurns: skip closed session",
				zap.String("turn_id", turn.ID.String()),
				zap.String("session_id", sessionID))
			// Still mark the turn as failed/interrupted so it leaves the stale state.
			if turn.Status != string(model.TurnStatusInterrupted) {
				if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusFailed, "server restart recovery (session closed)"); err != nil {
					logger.Error(ctx, "[Server] recoverStaleTurns: update stale turn failed (session closed)",
						zap.String("turn_id", turn.ID.String()),
						zap.String("session_id", sessionID),
						zap.Error(err))
				}
			}
			continue
		}

		sessionIDs[sessionID] = true

		// Mark as interrupted (if not already)
		if turn.Status != string(model.TurnStatusInterrupted) {
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusInterrupted, "server restart recovery"); err != nil {
				logger.Error(ctx, "[Server] recoverStaleTurns: update status",
					zap.String("turn_id", turn.ID.String()),
					zap.Error(err))
				continue
			}
		}

		// Publish resume work item with InterruptID if available
		if s.queue != nil {
			// Build payload with InterruptID for proper ResumeParams construction
			interruptID := turn.InterruptID
			payloadBytes, err := json.Marshal(&resumeWorkPayload{
				Kind:        "resume",
				SessionID:   turn.SessionID.String(),
				InterruptID: interruptID,
			})
			if err != nil {
				logger.Error(ctx, "[Server] recoverStaleTurns: marshal resume payload",
					zap.String("turn_id", turn.ID.String()),
					zap.Error(err))
				continue
			}
			payload := string(payloadBytes)

			if _, err := s.queue.Publish(ctx, turn.SessionID.String(), payload, 100); err != nil {
				logger.Error(ctx, "[Server] recoverStaleTurns: publish resume",
					zap.String("turn_id", turn.ID.String()),
					zap.Error(err))
			} else {
				logger.Info(ctx, "[Server] recoverStaleTurns: published resume",
					zap.String("turn_id", turn.ID.String()),
					zap.String("session_id", turn.SessionID.String()),
					zap.String("interrupt_id", interruptID))
			}
		}
	}

	// Clean up ghost work items and release stale session locks.
	// Ghost work: work items left in "processing" state after worker crash.
	// We detect them by checking if session:active pointer exists but lock expired.
	if s.queue != nil {
		sessionIDList := make([]string, 0, len(sessionIDs))
		for sid := range sessionIDs {
			sessionIDList = append(sessionIDList, sid)
		}

		// Batch requeue ghost works (single pipeline round-trip)
		requeued, err := s.queue.RequeueGhostWorksBatch(ctx, sessionIDList)
		if err != nil {
			logger.Warn(ctx, "[Server] recoverStaleTurns: batch requeue ghost works",
				zap.Error(err))
		} else {
			for sid, workID := range requeued {
				logger.Info(ctx, "[Server] recoverStaleTurns: requeued ghost work",
					zap.String("session_id", sid),
					zap.String("work_id", workID))
			}
		}

		// Release session locks (if still held)
		for _, sessionID := range sessionIDList {
			if err := s.queue.ReleaseSession(ctx, sessionID); err != nil {
				logger.Warn(ctx, "[Server] recoverStaleTurns: release session lock",
					zap.String("session_id", sessionID),
					zap.Error(err))
			} else {
				logger.Info(ctx, "[Server] recoverStaleTurns: released session lock",
					zap.String("session_id", sessionID))
			}
		}
	}
}

// staleTurnScanner runs periodically to recover stale turns that got stuck
// during runtime (e.g., worker crash, network partition). Uses a Redis
// distributed lock to ensure only one Server instance scans at a time.
func (s *Server) staleTurnScanner(ctx context.Context) {
	const (
		scannerInterval = 5 * time.Minute
		scannerLockKey  = "stale_turn_scanner_lock"
		scannerLockTTL  = 4 * time.Minute // < 5min interval
	)

	ticker := time.NewTicker(scannerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Redis distributed lock: only one instance scans at a time.
			acquired, err := s.svcCtx.Redis.SetNX(ctx, scannerLockKey,
				s.instanceID, scannerLockTTL).Result()
			if err != nil {
				logger.Warn(ctx, "[Server] staleTurnScanner: lock acquisition failed",
					zap.Error(err))
				continue // Redis failure: skip this round
			}
			if !acquired {
				if logger.IsDebugMode() {
					logger.Debug(ctx, "[Server] staleTurnScanner: lock held by another instance")
				}
				continue
			}

			s.periodicRecoverStaleTurns(ctx)
		}
	}
}

// periodicRecoverStaleTurns scans for stale turns and recovers them based on
// time thresholds and Worker liveness. Called by the periodic staleTurnScanner.
//
// Differences from recoverStaleTurns (startup recovery):
//   - Applies time thresholds (running > 10min, pending > 2min, interrupted > 30min).
//   - Checks Worker liveness (session lock TTL) and checkpoint existence before recovery.
//   - Per-status transitions: running → interrupted/failed, pending → failed, interrupted → cancelled.
//   - Publishes kind="submit" via publishRecoveryWorkItem (not "resume").
//   - Records Prometheus metrics and syncs session status after recovery.
//   - Uses LIMIT 100 (startup uses no limit).
func (s *Server) periodicRecoverStaleTurns(ctx context.Context) {
	const (
		runningThreshold     = 10 * time.Minute
		pendingThreshold     = 2 * time.Minute
		interruptedThreshold = 30 * time.Minute
		scanLimit            = 100
	)

	staleTurns, err := s.svcCtx.TurnRepo.FindStaleTurnsWithLimit(ctx, staleTurnStatuses, scanLimit)
	if err != nil {
		logger.Error(ctx, "[Server] periodicRecoverStaleTurns: find stale turns", zap.Error(err))
		return
	}

	if len(staleTurns) == 0 {
		return
	}

	logger.Info(ctx, "[Server] periodicRecoverStaleTurns: found stale turns",
		zap.Int("count", len(staleTurns)))

	now := time.Now()

	for _, turn := range staleTurns {
		sessionID := turn.SessionID.String()

		// Determine age based on status:
		// - running: use StartedAt (when it began executing)
		// - pending/interrupted: use CreatedAt
		var age time.Duration
		switch turn.Status {
		case string(model.TurnStatusRunning):
			if turn.StartedAt != nil {
				age = now.Sub(*turn.StartedAt)
			} else {
				age = now.Sub(turn.CreatedAt)
			}
			if age < runningThreshold {
				continue
			}
			// Check if a Worker is still alive (holding the session lock).
			// If so, the turn may still be progressing normally.
			if s.isWorkerAliveForSession(ctx, sessionID) {
				continue
			}
			// Check if checkpoint still exists. If expired, resume will fail,
			// so mark as failed directly.
			checkpointKey := cache.Checkpoint("session:" + sessionID)
			exists, err := s.svcCtx.Redis.Exists(ctx, checkpointKey).Result()
			if err != nil || exists == 0 {
				logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: checkpoint expired",
					zap.String("turn_id", turn.ID.String()),
					zap.String("session_id", sessionID))
				if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusFailed, "periodic scanner: checkpoint expired"); err != nil {
					logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update failed",
						zap.String("turn_id", turn.ID.String()), zap.Error(err))
				} else {
					s.syncSessionStatusAfterRecovery(ctx, turn)
					s.recordStaleTurnRecovery("running")
				}
				continue
			}
			logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: recovering stale running turn",
				zap.String("turn_id", turn.ID.String()),
				zap.Duration("age", age))
			if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, model.TurnStatusInterrupted, "periodic scanner: stale running turn"); err != nil {
				logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update interrupted",
					zap.String("turn_id", turn.ID.String()), zap.Error(err))
				continue
			}
			// Publish recovery work item. If publish fails, the turn is left in
			// interrupted state with no automatic recovery path — sync session
			// status to idle so the frontend is not stuck showing "active".
			if err := s.publishRecoveryWorkItem(ctx, turn); err != nil {
				s.syncSessionStatusAfterRecovery(ctx, turn)
			}
			s.recordStaleTurnRecovery("running")

		case string(model.TurnStatusPending):
			s.recoverSimpleStaleTurn(ctx, turn, now, pendingThreshold, model.TurnStatusFailed, "periodic scanner: stale pending turn", "pending")

		case string(model.TurnStatusInterrupted):
			s.recoverSimpleStaleTurn(ctx, turn, now, interruptedThreshold, model.TurnStatusCancelled, "periodic scanner: stale interrupted turn", "interrupted")
		}
	}
}

// recoverSimpleStaleTurn handles stale pending/interrupted turns by checking
// age threshold, updating status, syncing session, and recording recovery.
func (s *Server) recoverSimpleStaleTurn(ctx context.Context, turn *model.Turn, now time.Time, threshold time.Duration, targetStatus protocol.TurnStatus, reason, kind string) {
	age := now.Sub(turn.CreatedAt)
	if age < threshold {
		return
	}
	logger.Warn(ctx, "[Server] periodicRecoverStaleTurns: recovering stale "+kind+" turn",
		zap.String("turn_id", turn.ID.String()),
		zap.Duration("age", age))
	if err := s.svcCtx.TurnRepo.UpdateStatus(ctx, turn.ID, targetStatus, reason); err != nil {
		logger.Error(ctx, "[Server] periodicRecoverStaleTurns: update failed",
			zap.String("turn_id", turn.ID.String()), zap.Error(err))
		return
	}
	s.syncSessionStatusAfterRecovery(ctx, turn)
	s.recordStaleTurnRecovery(kind)
}

// isWorkerAliveForSession checks whether a Worker is holding the session lock.
// Uses the exported key accessor to avoid duplicating the Redis key format.
func (s *Server) isWorkerAliveForSession(ctx context.Context, sessionID string) bool {
	lockKey := rtcqueue.SessionLockKey(sessionID)
	ttl, err := s.svcCtx.Redis.TTL(ctx, lockKey).Result()
	if err != nil {
		return false
	}
	return ttl > 0
}

// publishRecoveryWorkItem publishes a kind="submit" work item to trigger turn
// recovery. Uses submit (not resume) to avoid state race conditions.
// Returns an error if the publish fails so the caller can take fallback action
// (e.g., sync session status to idle).
func (s *Server) publishRecoveryWorkItem(ctx context.Context, turn *model.Turn) error {
	if s.queue == nil {
		return nil
	}
	payload := string(turnagent.MarshalSubmitPayload(turn.SessionID.String(), 0))
	if _, err := s.queue.Publish(ctx, turn.SessionID.String(), payload, 100); err != nil {
		logger.Error(ctx, "[Server] publishRecoveryWorkItem: publish failed",
			zap.String("turn_id", turn.ID.String()),
			zap.Error(err))
		return err
	}
	logger.Info(ctx, "[Server] publishRecoveryWorkItem: published",
		zap.String("turn_id", turn.ID.String()),
		zap.String("session_id", turn.SessionID.String()))
	return nil
}

// syncSessionStatusAfterRecovery updates Session status to idle when the last
// running turn for a session has been recovered. Also publishes a session.updated
// event to the frontend.
func (s *Server) syncSessionStatusAfterRecovery(ctx context.Context, turn *model.Turn) {
	session, err := s.svcCtx.SessionRepo.GetByID(ctx, turn.SessionID)
	if err != nil {
		logger.Warn(ctx, "[Server] syncSessionStatusAfterRecovery: load session failed",
			zap.String("session_id", turn.SessionID.String()), zap.Error(err))
		return
	}
	if session.Status != string(model.SessionStatusActive) {
		return
	}
	// Check if any other running turns exist for this session.
	runningCount, err := s.svcCtx.TurnRepo.CountBySessionAndStatus(ctx, turn.SessionID, string(model.TurnStatusRunning))
	if err != nil {
		logger.Warn(ctx, "[Server] syncSessionStatusAfterRecovery: count running failed",
			zap.String("session_id", turn.SessionID.String()), zap.Error(err))
		return
	}
	if runningCount > 0 {
		return // other turns still running
	}
	if err := s.svcCtx.SessionRepo.UpdateStatus(ctx, turn.SessionID, model.SessionStatusIdle); err != nil {
		logger.Error(ctx, "[Server] syncSessionStatusAfterRecovery: update session failed",
			zap.String("session_id", turn.SessionID.String()), zap.Error(err))
		return
	}
	// Publish session.updated event to frontend.
	s.publishSessionStatusUpdate(ctx, session)
}

// publishSessionStatusUpdate publishes a session.updated event after the
// scanner modifies session status.
func (s *Server) publishSessionStatusUpdate(ctx context.Context, session *model.Session) {
	if s.svcCtx.UpdatePublisher == nil {
		return
	}
	updates := primitives.BuildSessionUpdatedUpdates(session)
	if len(updates) == 0 {
		return
	}
	if _, err := s.svcCtx.UpdatePublisher.Publish(ctx, updates...); err != nil {
		logger.Warn(ctx, "[Server] publishSessionStatusUpdate: publish failed",
			zap.String("session_id", session.ID.String()), zap.Error(err))
	}
}

// recordStaleTurnRecovery records a stale turn recovery metric.
func (s *Server) recordStaleTurnRecovery(status string) {
	if s.metrics == nil {
		return
	}
	s.metrics.RecordStaleTurnRecovery(context.Background(), turnagent.StaleTurnRecoveryAttrs{
		Status: status,
	})
}
