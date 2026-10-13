// internal/handler/rpc/action.submitrtcresult_resume.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/converter"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// tryCollectBatchResume attempts to collect batch resume items via Redis.
// Returns (items, true) if the batch is still in progress (caller should return),
// or (items, false) if the caller should continue with normal resume logic.
func (h *Handler) tryCollectBatchResume(ctx context.Context, rtc *converter.Rtc) ([]turnagent.BatchResumeItem, bool) {
	if h.deps.Deps.RtcBatchResumeUsecase == nil {
		return nil, false
	}

	resultStr := string(rtc.Result)
	if rtc.Status == string(protocol.RtcStatusFailed) && rtc.ErrorMessage != "" {
		resultStr = rtc.ErrorMessage
	}

	result, err := h.deps.Deps.RtcBatchResumeUsecase.TryCollectBatchResume(
		ctx,
		rtc.ID.String(),
		rtc.TurnID.String(),
		string(rtc.Result),
		rtc.Status,
		resultStr,
	)
	if err != nil {
		logger.Warn(ctx, "[resumeTurnAfterRtc] batch resume failed",
			zap.String("rtc", rtc.ID.String()),
			zap.String("turn", rtc.TurnID.String()),
			zap.Error(err))
		return nil, false
	}

	if !result.Exists {
		// Batch key doesn't exist (TTL expired or never created).
		return nil, false
	}

	if result.InProgress {
		// More RTCs to go.
		return nil, true
	}

	// All RTCs completed — return the batch items.
	logger.Info(ctx, "[resumeTurnAfterRtc] batch complete, resuming turn",
		zap.String("turn", rtc.TurnID.String()),
		zap.String("session", rtc.SessionID.String()),
		zap.Int("batch_size", len(result.Items)))
	return result.Items, false
}

// resumeActiveTurn publishes a Resume work item for the active (interrupted) turn.
func (h *Handler) resumeActiveTurn(ctx context.Context, rtc *converter.Rtc, activeTurns []*converter.Turn, batchResumeItems []turnagent.BatchResumeItem, userID string) {
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

	// Extract trace context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)

	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:             turnagent.WorkKindResume,
		SessionID:        rtc.SessionID.String(),
		UserID:           userID,
		InterruptID:      interruptID,
		InterruptResult:  interruptResult,
		BatchResumeItems: batchResumeItems,
		TraceID:          traceID,
		SpanID:           spanID,
	})
	if marshalErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] marshal resume payload", zap.Error(marshalErr))
		return
	}
	// Generate deterministic work ID for idempotent publishing.
	// Use RTC ID instead of turn ID to ensure each RTC submission gets a unique work ID.
	// This prevents conflicts when the same turn is resumed multiple times
	// (e.g., when a resumed turn triggers another interrupt).
	// The same RTC's duplicate submissions will still be correctly deduplicated.
	workID := rtcqueue.GenerateDeterministicWorkID(
		rtc.SessionID.String(),
		"resume:"+rtc.ID.String(),
	)
	if _, err := h.deps.Queue.PublishWithID(ctx, rtc.SessionID.String(), workID, string(payload), rtcqueue.ResumeWorkPriority); err != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] Queue.PublishWithID resume failed",
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
			zap.String("work_id", workID),
			zap.Int("batch_size", len(batchResumeItems)))
	}
}

// resolveInterruptID finds the interrupted turn's InterruptID, retrying with
// exponential backoff if the turn is still in "running" state.
func (h *Handler) resolveInterruptID(ctx context.Context, rtc *converter.Rtc, activeTurns []*converter.Turn) (string, *string) {
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
		updatedTurn, repoErr := h.deps.Deps.RtcSessionUsecase.GetTurn(ctx, pendingTurnID)
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
func (h *Handler) publishOrphanSubmit(ctx context.Context, rtc *converter.Rtc, userID string) {
	if h.deps.Deps.RtcBatchResumeUsecase == nil {
		return
	}

	ok, setnxErr := h.deps.Deps.RtcBatchResumeUsecase.SetOrphanTrigger(ctx, rtc.ID.String(), h.deps.Deps.WorkerConfig.OrphanTriggerTTL)
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

	// Extract trace context for cross-process propagation.
	traceID, spanID := turnagent.ExtractTraceFromCtx(ctx)

	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: rtc.SessionID.String(),
		UserID:    userID,
		TraceID:   traceID,
		SpanID:    spanID,
	})
	if marshalErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] marshal submit payload", zap.Error(marshalErr))
		return
	}
	// Generate deterministic work ID for idempotent publishing.
	// This ensures retries don't create duplicate submit work items.
	workID := rtcqueue.GenerateDeterministicWorkID(
		rtc.SessionID.String(),
		"orphan:"+rtc.ID.String(),
	)
	if _, err := h.deps.Queue.PublishWithID(ctx, rtc.SessionID.String(), workID, string(payload), rtcqueue.SubmitWorkPriority); err != nil {
		if delErr := h.deps.Deps.RtcBatchResumeUsecase.DeleteOrphanTrigger(ctx, rtc.ID.String()); delErr != nil {
			logger.Warn(ctx, "[resumeTurnAfterRtc] failed to del orphan key",
				zap.String("rtc", rtc.ID.String()), zap.Error(delErr))
		}
		logger.Error(ctx, "[resumeTurnAfterRtc] Queue.PublishWithID submit failed",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
	} else {
		logger.Info(ctx, "[resumeTurnAfterRtc] orphan submit published",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()),
			zap.String("work_id", workID))
	}
}
