// internal/handler/rpc/action.submitrtcresult_resume.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// tryCollectBatchResume attempts to collect batch resume items via Redis.
// Returns (items, true) if the batch is still in progress (caller should return),
// or (items, false) if the caller should continue with normal resume logic.
func (h *Handler) tryCollectBatchResume(ctx context.Context, rtc *model.Rtc) ([]turnagent.BatchResumeItem, bool) {
	if h.deps.Deps.Redis == nil {
		return nil, false
	}

	batchKey := cache.RtcBatchPending(rtc.TurnID.String())
	resultsKey := cache.RtcBatchResults(rtc.TurnID.String())
	interruptMapKey := cache.RtcBatchInterruptMap(rtc.TurnID.String())

	resultStr := string(rtc.Result)
	if rtc.Status == string(protocol.RtcStatusFailed) && rtc.ErrorMessage != "" {
		resultStr = rtc.ErrorMessage
	}

	remaining, scriptErr := cache.BatchComplete.Run(
		ctx, h.deps.Deps.Redis,
		[]string{batchKey, resultsKey},
		rtc.ID.String(), resultStr, 600,
	).Int64()

	if scriptErr != nil {
		logger.Warn(ctx, "[resumeTurnAfterRtc] batch complete script failed",
			zap.String("rtc", rtc.ID.String()),
			zap.String("turn", rtc.TurnID.String()),
			zap.Error(scriptErr))
		return nil, false
	}
	if remaining == -1 {
		// Batch key doesn't exist (TTL expired or never created).
		return nil, false
	}

	logger.Info(ctx, "[resumeTurnAfterRtc] batch progress",
		zap.String("rtc", rtc.ID.String()),
		zap.String("turn", rtc.TurnID.String()),
		zap.Int64("remaining", remaining))

	if remaining > 0 {
		return nil, true // More RTCs to go.
	}

	// All RTCs completed — build BatchResumeItems from stored data.
	return h.buildBatchResumeItems(ctx, rtc, resultsKey, interruptMapKey, batchKey), false
}

// buildBatchResumeItems reads batch results and interrupt map from Redis,
// then cleans up all batch-related keys.
func (h *Handler) buildBatchResumeItems(ctx context.Context, rtc *model.Rtc, resultsKey, interruptMapKey, batchKey string) []turnagent.BatchResumeItem {
	results, resultsErr := h.deps.Deps.Redis.HGetAll(ctx, resultsKey).Result()
	if resultsErr != nil {
		logger.Warn(ctx, "[resumeTurnAfterRtc] batch results read failed (degrading to single resume)",
			zap.String("turn", rtc.TurnID.String()), zap.Error(resultsErr))
	}
	interruptMap, mapErr := h.deps.Deps.Redis.HGetAll(ctx, interruptMapKey).Result()
	if mapErr != nil {
		logger.Warn(ctx, "[resumeTurnAfterRtc] batch interrupt map read failed (degrading to single resume)",
			zap.String("turn", rtc.TurnID.String()), zap.Error(mapErr))
	}

	items := make([]turnagent.BatchResumeItem, 0, len(interruptMap))
	for rtcID, interruptID := range interruptMap {
		result := results[rtcID]
		items = append(items, turnagent.BatchResumeItem{
			InterruptID: interruptID,
			Result:      result,
		})
		logger.Info(ctx, "[resumeTurnAfterRtc] batch item",
			zap.String("rtc_id", rtcID),
			zap.String("interrupt_id", interruptID),
			zap.Int("result_len", len(result)))
	}

	h.deps.Deps.Redis.Del(ctx, batchKey, resultsKey, interruptMapKey)
	logger.Info(ctx, "[resumeTurnAfterRtc] batch complete, resuming turn",
		zap.String("turn", rtc.TurnID.String()),
		zap.String("session", rtc.SessionID.String()),
		zap.Int("batch_size", len(items)))
	return items
}

// resumeActiveTurn publishes a Resume work item for the active (interrupted) turn.
func (h *Handler) resumeActiveTurn(ctx context.Context, rtc *model.Rtc, activeTurns []*model.Turn, batchResumeItems []turnagent.BatchResumeItem) {
	interruptID, interruptResult := h.resolveInterruptID(ctx, rtc, activeTurns)

	// Dedup check: skip if a Resume work item is already pending.
	if h.deps.Queue != nil {
		hasPending, checkErr := h.deps.Queue.HasPendingWorkByKind(
			ctx, rtc.SessionID.String(), string(turnagent.WorkKindResume),
		)
		if checkErr != nil {
			logger.Warn(ctx, "[resumeTurnAfterRtc] dedup check failed (proceeding anyway)",
				zap.String("rtc", rtc.ID.String()),
				zap.String("session", rtc.SessionID.String()),
				zap.Error(checkErr))
		} else if hasPending {
			logger.Info(ctx, "[resumeTurnAfterRtc] skip: resume work already pending",
				zap.String("rtc", rtc.ID.String()),
				zap.String("session", rtc.SessionID.String()),
				zap.String("turn", activeTurns[len(activeTurns)-1].ID.String()))
			return
		}
	}

	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:             turnagent.WorkKindResume,
		SessionID:        rtc.SessionID.String(),
		InterruptID:      interruptID,
		InterruptResult:  interruptResult,
		BatchResumeItems: batchResumeItems,
	})
	if marshalErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] marshal resume payload", zap.Error(marshalErr))
		return
	}
	if _, err := h.deps.Queue.Publish(ctx, rtc.SessionID.String(), string(payload), rtcqueue.ResumeWorkPriority); err != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] Queue.Publish resume failed",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
	} else {
		logMsg := "[resumeTurnAfterRtc] resume published"
		if len(batchResumeItems) > 0 {
			logMsg = "[resumeTurnAfterRtc] batch resume published"
		}
		logger.Info(ctx, logMsg,
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()),
			zap.String("turn", activeTurns[len(activeTurns)-1].ID.String()),
			zap.Int("batch_size", len(batchResumeItems)))
	}
}

// resolveInterruptID finds the interrupted turn's InterruptID, retrying with
// exponential backoff if the turn is still in "running" state.
func (h *Handler) resolveInterruptID(ctx context.Context, rtc *model.Rtc, activeTurns []*model.Turn) (string, *string) {
	// First pass: look for an already-interrupted turn.
	for _, t := range activeTurns {
		if protocol.TurnStatus(t.Status) == protocol.TurnStatusInterrupted {
			resultStr := string(rtc.Result)
			return t.InterruptID, &resultStr
		}
	}

	// Second pass: turn may still be "running" (interruptTurn hasn't persisted yet).
	var pendingTurnID uuid.UUID
	for _, t := range activeTurns {
		if protocol.TurnStatus(t.Status) == protocol.TurnStatusRunning ||
			protocol.TurnStatus(t.Status) == protocol.TurnStatusPending {
			pendingTurnID = t.ID
			break
		}
	}
	if pendingTurnID == uuid.Nil {
		return "", nil
	}

	// Retry with exponential backoff, waiting for interruptTurn to persist InterruptID.
	retryBO := backoff.NewExponentialBackOff()
	retryBO.InitialInterval = 20 * time.Millisecond
	retryBO.MaxInterval = 200 * time.Millisecond
	gotID, err := backoff.Retry(ctx, func() (string, error) {
		updatedTurn, repoErr := h.deps.Deps.TurnRepo.GetByID(ctx, pendingTurnID)
		if repoErr != nil {
			return "", backoff.Permanent(repoErr)
		}
		if protocol.TurnStatus(updatedTurn.Status) == protocol.TurnStatusInterrupted && updatedTurn.InterruptID != "" {
			return updatedTurn.InterruptID, nil
		}
		return "", errors.New("interrupt not ready")
	},
		backoff.WithBackOff(retryBO),
		backoff.WithMaxTries(10),
	)
	if err != nil || gotID == "" {
		logger.Warn(ctx, "[resumeTurnAfterRtc] InterruptID not available after retry",
			zap.String("turn", pendingTurnID.String()),
			zap.String("session", rtc.SessionID.String()))
		return "", nil
	}
	resultStr := string(rtc.Result)
	return gotID, &resultStr
}

// publishOrphanSubmit publishes a Submit work item when no active turn exists.
func (h *Handler) publishOrphanSubmit(ctx context.Context, rtc *model.Rtc) {
	orphanKey := cache.RtcOrphanTriggered(rtc.ID.String())
	ok, setnxErr := h.deps.Deps.Redis.SetNX(ctx, orphanKey, "1", h.deps.Deps.WorkerConfig.OrphanTriggerTTL).Result()
	if setnxErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] setnx orphan key",
			zap.String("rtc", rtc.ID.String()), zap.Error(setnxErr))
		return
	}
	if !ok {
		logger.Info(ctx, "[resumeTurnAfterRtc] skip orphan: already triggered",
			zap.String("rtc", rtc.ID.String()))
		return
	}

	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: rtc.SessionID.String(),
	})
	if marshalErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] marshal submit payload", zap.Error(marshalErr))
		return
	}
	if _, err := h.deps.Queue.Publish(ctx, rtc.SessionID.String(), string(payload), 0); err != nil {
		if delErr := h.deps.Deps.Redis.Del(ctx, orphanKey).Err(); delErr != nil {
			logger.Warn(ctx, "[resumeTurnAfterRtc] failed to del orphan key",
				zap.String("key", orphanKey), zap.Error(delErr))
		}
		logger.Error(ctx, "[resumeTurnAfterRtc] Queue.Publish submit failed",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
	} else {
		logger.Info(ctx, "[resumeTurnAfterRtc] orphan submit published",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()))
	}
}
