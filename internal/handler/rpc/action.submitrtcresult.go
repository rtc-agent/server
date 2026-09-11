// internal/handler/rpc/action.submitrtcresult.go
package rpchandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	turnagent "github.com/rtc-agent/server/pkg/turn-agent"
	"go.uber.org/zap"
)

// ResumePriority is the priority used for resume work items. rtc-queue's
// priority queue orders by score = -priority, so higher values are claimed
// first. A resume must always outrank a fresh submit to ensure the
// checkpoint is still intact when the worker picks it up.
const ResumePriority int64 = 100

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
		zap.String("rtc", string(req.RtcId)),
		zap.Bool("success", req.Success))

	// 加载 RTC 并校验存在
	rtc, err := h.deps.Deps.RtcRepo.GetByID(ctx, rtcUUID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil, &APIError{Code: "rtc.not_found", Message: fmt.Sprintf("rtc %s not found", req.RtcId)}
		}
		return nil, h.internalError(ctx, "rtc.error", "internal error", err)
	}

	// 幂等校验（spec Section 8）
	switch protocol.RtcStatus(rtc.Status) {
	case protocol.RtcStatusCompleted, protocol.RtcStatusFailed,
		protocol.RtcStatusTimeout, protocol.RtcStatusRejected:
		// 终态 + 同一 ClientID → 视为重复上报，接受并返回成功
		if req.ClientId != nil && *req.ClientId == rtc.ClientID {
			logger.Info(ctx, "[SubmitRtcResult] idempotent repeat",
				zap.String("rtc", string(req.RtcId)),
				zap.String("status", rtc.Status),
				zap.String("client_id", rtc.ClientID))
			return &protocol.SubmitRtcResultResponse{
				Result: protocol.SubmitRtcResultResult{Success: true},
			}, nil
		}
		// 终态 + 不同 ClientID → 冲突，返回已有状态
		ups := make([]protocol.Update, 0)
		ups = append(ups, protocol.Update{
			DataList: new([]interface{}{
				model.ToProtocolRtc(rtc),
			}),
			Id: uuid.NewString(),
			Items: []protocol.UpdateItem{protocol.UpdateItem{
				Action:   protocol.ActionUpdated,
				Entity:   protocol.EntityRtc,
				EntityId: rtc.ID.String(),
			}},
			Offset: 0,
		})
		return &protocol.SubmitRtcResultResponse{
			Result:  protocol.SubmitRtcResultResult{Success: true},
			Updates: &ups,
		}, nil
	}

	// 归属校验：通过 RTC 的 sessionID 校验用户权限
	if err := primitives.CheckSessionOwnership(ctx, h.deps.Deps, rtc.SessionID, creator); err != nil {
		return nil, h.ownershipError(ctx, err)
	}

	// ClientID 校验：如果客户端传了 ClientID，必须与 RTC 的 ClientID 匹配
	if req.ClientId != nil && *req.ClientId != rtc.ClientID {
		return nil, &APIError{
			Code:    "rtc.client_id_mismatch",
			Message: fmt.Sprintf("client_id mismatch: expected %s, got %s", rtc.ClientID, *req.ClientId),
		}
	}

	// 计算目标状态：成功 → completed，失败 → failed
	targetStatus := protocol.RtcStatusCompleted
	if !req.Success {
		targetStatus = protocol.RtcStatusFailed
	}

	// 序列化 result 为 JSON 字符串（dbmodel 层存储为 jsonb）
	// 使用 *string 以便在 result 为空时存储 NULL（jsonb 不接受空字符串）
	var resultJSON *string
	if req.Result != nil {
		b, err := json.Marshal(req.Result)
		if err != nil {
			return nil, &APIError{Code: "rtc.invalid_result", Message: fmt.Sprintf("marshal result: %v", err)}
		}
		s := string(b)
		resultJSON = &s
	}

	// Load session BEFORE the transaction for event publishing.
	//
	// Why old data? Event publishing is best-effort — the update items only
	// need the session's OwnerRefID to route the update to the correct user
	// topic. If we reload the session inside the transaction and the reload
	// fails (transient DB error), we would fail the entire transaction for
	// a non-critical concern.
	//
	// Using pre-transaction data is fine: the frontend will reload the
	// latest state when it receives the event anyway. The OwnerRefID (used
	// for routing) never changes after session creation.
	//
	// If the pre-load itself fails, we still proceed with the update — we
	// just won't have OwnerRefID for routing and the event won't be
	// published. The frontend can poll for the latest state.
	sessionBefore, sessionLoadErr := h.deps.SessionRepo.GetByID(ctx, rtc.SessionID)
	if sessionLoadErr != nil {
		logger.Warn(ctx, "[SubmitRtcResult] pre-load session failed (will use best-effort event publishing)",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(sessionLoadErr))
	}

	pushUpdates, err := h.deps.Deps.UpdatePublisher.RunAndPublish(ctx, func(txCtx context.Context) ([]updates.UpdatePublishItem, error) {
		if err := primitives.UpdateRtcResult(txCtx, h.deps.Deps, rtcUUID, targetStatus, resultJSON, model.DerefStr(req.Error)); err != nil {
			return nil, err
		}

		// 查找 toolcall_input 消息以获取 ToolCall.Id（即 eino 的 tool_call_id）
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

		// 构造 toolcall_output ContentData
		toolOutput := ""
		if resultJSON != nil {
			toolOutput = *resultJSON
		} else if req.Error != nil {
			toolOutput = *req.Error
		}
		targetStatusStr := string(targetStatus)
		outputToolCall := protocol.ToolCall{
			Id:       inputToolCall.Id, // 与 toolcall_input 保持一致的 tool_call_id
			ToolName: rtc.ToolName,
			Input:    inputToolCall.Input,
			Output:   &toolOutput,
			Status:   &targetStatusStr,
		}
		outputContentData := protocol.ContentData{
			Type: protocol.ContentTypeToolCallOutput,
			Data: outputToolCall,
		}

		// 创建 toolcall_output Message
		parentMsgID := rtc.MessageID
		outputMsg, createErr := primitives.CreateMessage(
			txCtx, h.deps.Deps,
			rtc.SessionID, &rtc.TurnID,
			protocol.MessageRoleTool,
			usecase.SystemCreator{}, // system-generated
			outputContentData,
			protocol.MessageStreamingCompleted,
			"",           // system-generated
			&parentMsgID, // parent message is toolcall_input
		)
		if createErr != nil {
			return nil, fmt.Errorf("create toolcall_output message: %w", createErr)
		}

		// 回填 RTC 的 OutputMessageID
		if err := h.deps.Deps.RtcRepo.UpdateOutputMessageID(txCtx, rtcUUID, outputMsg.ID); err != nil {
			logger.Error(txCtx, "[SubmitRtcResult] update output_message_id",
				zap.String("rtc", rtcUUID.String()),
				zap.Error(err))
			// 不阻塞主流程
		}

		// Use the session loaded BEFORE the transaction for event publishing.
		// If pre-load failed, we can still build updates with a nil session —
		// the update publisher will skip publishing for nil sessions.
		//
		// We deliberately do NOT reload the session here inside the
		// transaction: the reload is for routing metadata (OwnerRefID), which
		// never changes after session creation. A transient reload failure
		// must not abort the RTC result update.
		session := sessionBefore

		// 构建 updates：RTC result + toolcall_output message created
		items := primitives.BuildRtcResultUpdates(session, rtcUUID)
		if len(items) > 0 {
			items[0].Items = append(items[0].Items, protocol.UpdateItem{
				Entity:   protocol.EntityMessage,
				Action:   protocol.ActionCreated,
				EntityId: protocol.UUID(outputMsg.ID.String()),
			})
		}
		return items, nil
	})
	if err != nil {
		if errors.Is(err, updates.ErrPushAfterCommit) {
			logger.Warn(ctx, "[SubmitRtcResult] push failed after commit (data safe)", zap.Error(err))
		} else {
			return nil, h.internalError(ctx, "rtc.error", "internal error", err)
		}
	}

	// Resume the interrupted turn via rtc-queue. The old architecture used
	// Redis SET+PUBLISH to wake a handleInterrupt goroutine; in the new
	// architecture we publish a Resume work item that the turn-agent picks
	// up via rtc-queue's claim loop.
	h.resumeTurnAfterRtc(ctx, rtc)

	return &protocol.SubmitRtcResultResponse{
		Result:  protocol.SubmitRtcResultResult{Success: true},
		Updates: updates.DerefUpdates(pushUpdates),
	}, nil
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
	ctx := context.WithoutCancel(callerCtx)
	if h.deps.Queue == nil {
		logger.Warn(ctx, "[resumeTurnAfterRtc] Queue is nil, cannot resume",
			zap.String("rtc", rtc.ID.String()))
		return
	}

	if logger.DebugMode {
		logger.Debug(ctx, "[resumeTurnAfterRtc] entry",
			zap.String("rtc", rtc.ID.String()),
			zap.String("session", rtc.SessionID.String()))
	}

	// === Batch Resume Logic ===
	// Check if this RTC is part of a batch. If so, atomically store the result,
	// remove the RTC from the pending set, and check if all RTCs have completed.
	// Only publish Resume when the pending set becomes empty.
	var batchResumeItems []turnagent.BatchResumeItem
	if h.deps.Deps.Redis != nil {
		batchKey := cache.RtcBatchPending(rtc.TurnID.String())
		resultsKey := cache.RtcBatchResults(rtc.TurnID.String())
		interruptMapKey := cache.RtcBatchInterruptMap(rtc.TurnID.String())

		// Build the result string for this RTC
		resultStr := string(rtc.Result)
		if rtc.Status == string(protocol.RtcStatusFailed) && rtc.ErrorMessage != "" {
			resultStr = rtc.ErrorMessage
		}

		// Atomically: check batch exists, store result, remove from pending, get remaining count.
		// Returns -1 if batch key doesn't exist (TTL expired or never created).
		remaining, scriptErr := cache.BatchComplete.Run(
			ctx, h.deps.Deps.Redis,
			[]string{batchKey, resultsKey},
			rtc.ID.String(), resultStr, 600, // 600s = 10min TTL on results key
		).Int64()

		if scriptErr != nil {
			logger.Warn(ctx, "[resumeTurnAfterRtc] batch complete script failed",
				zap.String("rtc", rtc.ID.String()),
				zap.String("turn", rtc.TurnID.String()),
				zap.Error(scriptErr))
			// Fall through to non-batch resume path
		} else if remaining == -1 {
			// Batch key doesn't exist (TTL expired or never created) — skip batch logic
		} else {
			logger.Info(ctx, "[resumeTurnAfterRtc] batch progress",
				zap.String("rtc", rtc.ID.String()),
				zap.String("turn", rtc.TurnID.String()),
				zap.Int64("remaining", remaining))

			if remaining > 0 {
				// More RTCs to go, don't resume yet
				return
			}

			// All RTCs completed! Build BatchResumeItems from stored data
			results, _ := h.deps.Deps.Redis.HGetAll(ctx, resultsKey).Result()
			interruptMap, _ := h.deps.Deps.Redis.HGetAll(ctx, interruptMapKey).Result()

			// Build BatchResumeItems: map each RTC ID to its interrupt ID and result
			for rtcID, interruptID := range interruptMap {
				result := results[rtcID]
				batchResumeItems = append(batchResumeItems, turnagent.BatchResumeItem{
					InterruptID: interruptID,
					Result:      result,
				})
				logger.Info(ctx, "[resumeTurnAfterRtc] batch item",
					zap.String("rtc_id", rtcID),
					zap.String("interrupt_id", interruptID),
					zap.String("result_len", fmt.Sprintf("%d", len(result))))
			}

			// Clean up all batch-related keys
			h.deps.Deps.Redis.Del(ctx, batchKey, resultsKey, interruptMapKey)
			logger.Info(ctx, "[resumeTurnAfterRtc] batch complete, resuming turn",
				zap.String("turn", rtc.TurnID.String()),
				zap.String("session", rtc.SessionID.String()),
				zap.Int("batch_size", len(batchResumeItems)))
			// Continue to the resume logic below with batchResumeItems populated
		}
	}

	// 0. 前置检查：session 已 closed 则不触发新 turn
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

	// 1. Check if there's an active turn for this session.
	activeTurns, err := h.deps.Deps.TurnRepo.FindActiveBySession(ctx, rtc.SessionID)
	if err != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] find active turns",
			zap.String("session", rtc.SessionID.String()),
			zap.Error(err))
		return
	}

	if len(activeTurns) > 0 {
		// Happy path: there's an active (most likely interrupted) turn.
		// Publish a Resume work item at high priority so it's claimed before
		// any pending Submit items.
		//
		// Find the interrupted turn to get its InterruptID for ResumeParams.
		// If the turn is still "running" (interruptTurn hasn't executed yet),
		// wait briefly for the InterruptID to be persisted.
		var interruptID string
		var interruptResult *string
		var interruptedTurnID uuid.UUID

		// First pass: look for an interrupted turn
		for _, t := range activeTurns {
			if protocol.TurnStatus(t.Status) == protocol.TurnStatusInterrupted {
				interruptID = t.InterruptID
				interruptedTurnID = t.ID
				// Convert RTC result to string for the interrupt result.
				// Even if rtc.Result is empty, we set interruptResult to a non-nil
				// value (empty string) so the interrupted tool knows it was resumed.
				// A nil result would make compose.GetResumeContext unable to distinguish
				// between "no result provided" and "not resumed yet".
				resultStr := string(rtc.Result)
				interruptResult = &resultStr
				break
			}
		}

		// If no interrupted turn found, the turn may still be "running"
		// (interruptTurn hasn't persisted InterruptID yet). Wait briefly.
		if interruptID == "" {
			for _, t := range activeTurns {
				if protocol.TurnStatus(t.Status) == protocol.TurnStatusRunning ||
					protocol.TurnStatus(t.Status) == protocol.TurnStatusPending {
					interruptedTurnID = t.ID
					break
				}
			}

			if interruptedTurnID != uuid.Nil {
				// Retry a few times, waiting for interruptTurn to persist InterruptID
				for attempt := 0; attempt < 10; attempt++ {
					time.Sleep(50 * time.Millisecond)
					updatedTurn, err := h.deps.Deps.TurnRepo.GetByID(ctx, interruptedTurnID)
					if err != nil {
						break
					}
					if protocol.TurnStatus(updatedTurn.Status) == protocol.TurnStatusInterrupted && updatedTurn.InterruptID != "" {
						interruptID = updatedTurn.InterruptID
						// Always set interruptResult (even if empty) so the tool
						// receives a non-nil resume result.
						resultStr := string(rtc.Result)
						interruptResult = &resultStr
						break
					}
				}
				if interruptID == "" {
					logger.Warn(ctx, "[resumeTurnAfterRtc] InterruptID not available after retry",
						zap.String("turn", interruptedTurnID.String()),
						zap.String("session", rtc.SessionID.String()))
				}
			}
		}

		// Dedup check: if a Resume work item is already pending or processing
		// for this session, skip publishing another one. Without this check,
		// concurrent SubmitRtcResult calls (e.g. network retries) would each
		// publish a Resume work item, causing the AI to load the same
		// conversation history and execute the same task multiple times.
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
			BatchResumeItems: batchResumeItems, // Use batch items if available
		})
		if marshalErr != nil {
			logger.Error(ctx, "[resumeTurnAfterRtc] marshal resume payload", zap.Error(marshalErr))
			return
		}
		if _, err := h.deps.Queue.Publish(ctx, rtc.SessionID.String(), string(payload), ResumePriority); err != nil {
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
		return
	}

	// 2. Orphan path: no active turn (worker crash or turn already terminal).
	// Use SETNX idempotency to ensure we only trigger one orphan recovery per RTC.
	orphanKey := cache.RtcOrphanTriggered(rtc.ID.String())
	ok, setnxErr := h.deps.Deps.Redis.SetNX(ctx, orphanKey, "1", h.deps.Deps.WorkerConfig.OrphanTriggerTTL).Result()
	if setnxErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] setnx orphan key",
			zap.String("rtc", rtc.ID.String()),
			zap.Error(setnxErr))
		return
	}
	if !ok {
		logger.Info(ctx, "[resumeTurnAfterRtc] skip orphan: already triggered",
			zap.String("rtc", rtc.ID.String()))
		return
	}

	// Do NOT pre-create the turn here. The turn is created by turn-agent's
	// CreateTurn callback when the worker picks up the work item from rtc-queue.
	// This ensures the turn lifecycle is managed by turn-agent, not the API layer.
	//
	// The RTC result message is already in the DB (created earlier in this function).
	// When turn-agent loads messages (via LoadMessages callback), it will see the
	// RTC result. The tool can then check the RTC status and proceed accordingly.

	// Publish Submit work item. turn-agent will create the turn when processing it.
	payload, marshalErr := json.Marshal(turnagent.WorkPayload{
		Kind:      turnagent.WorkKindSubmit,
		SessionID: rtc.SessionID.String(),
	})
	if marshalErr != nil {
		logger.Error(ctx, "[resumeTurnAfterRtc] marshal submit payload", zap.Error(marshalErr))
		return
	}
	if _, err := h.deps.Queue.Publish(ctx, rtc.SessionID.String(), string(payload), 0); err != nil {
		// Roll back orphan key to allow retry
		if delErr := h.deps.Deps.Redis.Del(ctx, orphanKey).Err(); delErr != nil {
			logger.Warn(ctx, "[resumeTurnAfterRtc] failed to del orphan key",
				zap.String("key", orphanKey),
				zap.Error(delErr))
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
