// internal/handler/rpc/action.submitrtcresult.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/google/uuid"
	"github.com/rtc-agent/server/internal/infra/cache"
	"github.com/rtc-agent/server/internal/infra/contextx"
	"github.com/rtc-agent/server/internal/model"
	"github.com/rtc-agent/server/internal/repo"
	"github.com/rtc-agent/server/internal/updates"
	"github.com/rtc-agent/server/internal/usecase"
	"github.com/rtc-agent/server/internal/usecase/primitives"
	"github.com/rtc-agent/server/pkg/logger"
	"github.com/rtc-agent/server/pkg/protocol"
	rtcqueue "github.com/rtc-agent/server/pkg/rtc-queue"
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// SubmitRtcResult 提交 RTC 执行结果，标记 RTC 完成并继续 LLM 流程。
func (h *Handler) SubmitRtcResult(ctx context.Context, req *protocol.SubmitRtcResultRequest) (*protocol.SubmitRtcResultResponse, error) {
	userID, ok := contextx.GetUserID(ctx)
	if !ok {
		return nil, &APIError{Code: "unauthorized", Message: "missing user_id in context"}
	}
	creator := usecase.UserCreator{UserID: userID}

	rtcUUID, apiErr := parseUUID(req.RtcId, "rtc_id")
	if apiErr != nil {
		return nil, apiErr
	}

	logger.Info(ctx, "[SubmitRtcResult]",
		zap.String("user", userID.String()),
		zap.String("rtc", req.RtcId),
		zap.Bool("success", req.Success))

	rtc, err := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "rtc.not_found", Message: fmt.Sprintf("rtc %s not found", req.RtcId)}
		}
		return nil, h.internalError(ctx, "rtc.error", "internal error", err)
	}

	// Idempotency check for terminal-state RTCs.
	if idempotentResp, isIdempotent := h.checkIdempotentSubmit(ctx, rtc, req); isIdempotent {
		return idempotentResp, nil
	}

	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, rtc.SessionID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}

	if req.ClientId != nil && *req.ClientId != rtc.ClientID {
		return nil, &APIError{
			Code:    "rtc.client_id_mismatch",
			Message: fmt.Sprintf("client_id mismatch: expected %s, got %s", rtc.ClientID, *req.ClientId),
		}
	}

	targetStatus := protocol.RtcStatusCompleted
	if !req.Success {
		targetStatus = protocol.RtcStatusFailed
	}

	var resultJSON *string
	if req.Result != nil {
		b, marshalErr := json.Marshal(req.Result)
		if marshalErr != nil {
			return nil, &APIError{Code: "rtc.invalid_result", Message: fmt.Sprintf("marshal result: %v", marshalErr)}
		}
		s := string(b)
		resultJSON = &s
	}

	// Pre-load session for event routing (best-effort, not critical).
	sessionBefore, sessionLoadErr := h.deps.SessionRepo.GetByID(ctx, rtc.SessionID)
	if sessionLoadErr != nil {
		logger.Warn(ctx, "[SubmitRtcResult] pre-load session failed (will use best-effort event publishing)",
			zap.String("session", rtc.SessionID.String()), zap.Error(sessionLoadErr))
	}

	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		return h.updateRtcAndCreateOutput(txCtx, rtc, rtcUUID, targetStatus, resultJSON, req.Error, sessionBefore)
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[SubmitRtcResult] push failed after commit (data safe)", zap.Error(err))
		} else {
			return nil, h.internalError(ctx, "rtc.error", "internal error", err)
		}
	}

	if rtc.ToolName == "script" {
		recorderCtx, recorderCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer recorderCancel()
		h.recorder.submit(recorderCtx, rtc, req)
	}

	h.resumeTurnAfterRtc(ctx, rtc)

	return &protocol.SubmitRtcResultResponse{
		Result:  protocol.SubmitRtcResultResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
}

// checkIdempotentSubmit handles the idempotency check for RTCs already in a terminal state.
// Returns (response, true) if the request was handled idempotently, or (nil, false) to continue.
func (h *Handler) checkIdempotentSubmit(ctx context.Context, rtc *model.Rtc, req *protocol.SubmitRtcResultRequest) (*protocol.SubmitRtcResultResponse, bool) {
	switch protocol.RtcStatus(rtc.Status) {
	case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		if req.ClientId != nil && *req.ClientId == rtc.ClientID {
			logger.Info(ctx, "[SubmitRtcResult] idempotent repeat",
				zap.String("rtc", req.RtcId),
				zap.String("status", rtc.Status),
				zap.String("client_id", rtc.ClientID))
			return &protocol.SubmitRtcResultResponse{
				Result: protocol.SubmitRtcResultResult{Success: true},
			}, true
		}
		ups := []protocol.Update{{
			DataList: new([]interface{}{model.ToProtocolRtc(rtc)}),
			Id:       uuid.Must(uuid.NewV7()).String(),
			Items: []protocol.UpdateItem{{
				Action:   protocol.ActionUpdated,
				Entity:   protocol.EntityRtc,
				EntityId: rtc.ID.String(),
			}},
			Offset: 0,
		}}
		return &protocol.SubmitRtcResultResponse{
			Result:  protocol.SubmitRtcResultResult{Success: true},
			Updates: &ups,
		}, true
	}
	return nil, false
}

// updateRtcAndCreateOutput performs the transactional update: marks the RTC result,
// creates the toolcall_output message, and builds update items for event publishing.
func (h *Handler) updateRtcAndCreateOutput(txCtx context.Context, rtc *model.Rtc, rtcUUID uuid.UUID, targetStatus protocol.RtcStatus, resultJSON *string, reqError *string, session *model.Session) ([]updates.UpdatePublishItem, error) {
	if err := primitives.UpdateRtcResult(txCtx, h.deps.Deps, rtcUUID, targetStatus, resultJSON, model.DerefStr(reqError)); err != nil {
		return nil, err
	}

	inputMsg, err := h.deps.Deps.MessageRepo.GetByID(txCtx, rtc.MessageID)
	if err != nil {
		return nil, fmt.Errorf("get toolcall_input message %s: %w", rtc.MessageID, err)
	}
	inputContentData, err := primitives.ParseContentData(inputMsg.Content)
	if err != nil {
		return nil, fmt.Errorf("parse toolcall_input content: %w", err)
	}
	inputToolCall, err := primitives.ParseContentDataToolCall(inputContentData.Data)
	if err != nil {
		return nil, fmt.Errorf("parse toolcall_input data: %w", err)
	}

	toolOutput := ""
	if resultJSON != nil {
		toolOutput = *resultJSON
	} else if reqError != nil {
		toolOutput = *reqError
	}
	targetStatusStr := string(targetStatus)
	outputContentData := protocol.ContentData{
		Type: protocol.ContentTypeToolCallOutput,
		Data: protocol.ToolCall{
			Id:       inputToolCall.Id,
			ToolName: rtc.ToolName,
			Input:    inputToolCall.Input,
			Output:   &toolOutput,
			Status:   &targetStatusStr,
		},
	}

	parentMsgID := rtc.MessageID
	outputMsg, createErr := primitives.CreateMessage(
		txCtx, h.deps.Deps,
		rtc.SessionID, &rtc.TurnID,
		protocol.MessageRoleTool,
		usecase.SystemCreator{},
		outputContentData,
		protocol.MessageStreamingCompleted,
		"",
		&parentMsgID,
	)
	if createErr != nil {
		return nil, fmt.Errorf("create toolcall_output message: %w", createErr)
	}

	if err := h.deps.Deps.RtcRepo.UpdateOutputMessageID(txCtx, rtcUUID, outputMsg.ID); err != nil {
		logger.Error(txCtx, "[SubmitRtcResult] update output_message_id",
			zap.String("rtc", rtcUUID.String()), zap.Error(err))
	}

	items := primitives.BuildRtcResultUpdates(session, rtcUUID)
	if len(items) > 0 {
		items[0].Items = append(items[0].Items, protocol.UpdateItem{
			Entity:   protocol.EntityMessage,
			Action:   protocol.ActionCreated,
			EntityId: outputMsg.ID.String(),
		})
	}
	return items, nil
}

// resumeTurnAfterRtc publishes a Resume work item to rtc-queue so the
// turn-agent can continue the interrupted turn from its eino checkpoint.
//
// With batch resume: if this RTC is part of a batch (multiple RTCs in the
// same turn), we wait until all RTCs in the batch complete before publishing
// a single Resume work item. This prevents checkpoint corruption from
// multiple concurrent interrupts.
//
// If the turn that created the RTC is no longer active (e.g. worker crash,
// turn already terminal), we publish a Submit work item instead. The turn
// will be created by turn-agent's CreateTurn callback when the worker
// processes the work item — the API layer does NOT pre-create the turn.
//
// Errors are logged but never returned — the RTC result has already been
// persisted, and a failure here just means the agent won't auto-continue.
//
// The function detaches from the caller's context: the RPC handler's
// context may be cancelled when the RPC returns, but the resume/submit
// operations (DB queries, Redis SetNX, Queue.Publish) must complete
// independently since they are fire-and-forget.
func (h *Handler) resumeTurnAfterRtc(callerCtx context.Context, rtc *model.Rtc) {
	// Detach from the RPC handler's context. The RTC result is already
	// persisted; the resume is fire-and-forget and must not be aborted
	// by the RPC context timeout/cancellation.
	// 添加超时防止下游操作挂起导致 goroutine 泄漏
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 30*time.Second)
	defer cancel()
	if h.deps.Queue == nil {
		logger.Warn(ctx, "[resumeTurnAfterRtc] Queue is nil, cannot resume",
			zap.String("rtc", rtc.ID.String()))
		return
	}

	if logger.IsDebugMode() {
		logger.Debug(ctx, "[resumeTurnAfterRtc] entry",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()))
	}

	// === Batch Resume Logic ===
	batchResumeItems, batchDone := h.tryCollectBatchResume(ctx, rtc)
	if batchDone {
		// Batch still has pending RTCs; resume will happen when the last one completes.
		return
	}

	// 前置检查：session 已 closed 则不触发新 turn
	session, err := h.deps.SessionRepo.GetByID(ctx, rtc.SessionID)
	if err != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] get session",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
		return
	}
	if protocol.SessionStatus(session.Status) == protocol.SessionStatusClosed {
		logger.Info(ctx, "[resumeTurnAfterRtc] skip: session closed",
			zap.String("session", rtc.SessionID.String()))
		return
	}

	// Check if there's an active turn for this session.
	activeTurns, err := h.deps.Deps.TurnRepo.FindActiveBySession(ctx, rtc.SessionID)
	if err != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] find active turns",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
		return
	}

	if len(activeTurns) > 0 {
		h.resumeActiveTurn(ctx, rtc, activeTurns, batchResumeItems)
		return
	}

	// Orphan path: no active turn (worker crash or turn already terminal).
	h.publishOrphanSubmit(ctx, rtc)
}

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
